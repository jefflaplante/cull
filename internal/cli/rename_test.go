package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
)

// renameShoot is a folder named like an offload's with two loose frames.
func renameShoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "2026-10-04 Smith wedding")
	os.Mkdir(dir, 0o755)
	for i, n := range []string{"M1.DNG", "M2.DNG"} {
		b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", Payload: []byte{byte(i), 1, 2}})
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	return dir
}

func TestRenameCommand(t *testing.T) {
	dir := renameShoot(t)
	out, err := run(t, "rename", "--dry-run", dir, "{date}_{name}_{n:2}")
	if err != nil || !strings.Contains(out, "M1.DNG → 20251228_Smith_wedding_01.DNG") || !strings.Contains(out, "would rename 2") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "M1.DNG")); err != nil {
		t.Fatal("dry run renamed")
	}
	out, err = run(t, "rename", dir, "{date}_{name}_{n:2}")
	if err != nil || !strings.Contains(out, "renamed 2") || !strings.Contains(out, "Capture One or Lightroom") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "20251228_Smith_wedding_02.DNG")); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "rename", "--undo", dir)
	if err != nil || !strings.Contains(out, "put back 2") {
		t.Fatalf("undo: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "M2.DNG")); err != nil {
		t.Fatal(err)
	}
}

func TestRenameArgs(t *testing.T) {
	dir := renameShoot(t)
	for _, args := range [][]string{
		{"rename", dir},
		{"rename", "--undo", dir, "{n}"},
		{"rename", dir, "{name}"},
		{"rename", dir, "x/{n}"},
		{"rename", filepath.Join(dir, "keep"), "{n}"},
	} {
		if out, err := run(t, args...); err == nil {
			t.Errorf("%v: no error\n%s", args, out)
		}
	}
	if !notInDotfile["undo"] || !notInDotfile["reorder"] {
		t.Fatal("--undo or --reorder may be set in the dotfile")
	}
	if out, _ := run(t, "rename", "--help"); !strings.Contains(out, "--reorder") || !strings.Contains(out, "Capture One's settings") {
		t.Fatalf("help:\n%s", out)
	}
}

// While a rename is unfinished, judge, decide, review, restore and redate refuse,
// naming the command that finishes it; status points at it; and a rename refuses
// during an unfinished redate.
func TestUnfinishedRenameGuards(t *testing.T) {
	dir := statusShoot(t)
	j := &journal.Rename{Pattern: "{n:4}", Started: time.Now(), Phase: 1}
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"judge", "--backend", "openai", "--model", "m", dir},
		{"decide", dir},
		{"review", "--static", dir},
		{"restore", dir},
		{"redate", "--date", "2026-10-04", dir},
	} {
		out, err := run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "cull rename "+dir+" '{n:4}'") {
			t.Errorf("%v: err %v\n%s", args[0], err, out)
		}
	}
	out, err := run(t, "status", dir)
	if err != nil || !strings.Contains(out, "unfinished: a rename") || !strings.Contains(out, "next: cull rename "+dir+" '{n:4}'") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	os.Remove(filepath.Join(dir, journal.RenameName))
	(&journal.Redate{Target: "2026-10-04T12:00:00", Started: time.Now()}).Save(dir)
	if out, err := run(t, "rename", dir, "{n:4}"); err == nil || !strings.Contains(err.Error(), "cull redate") {
		t.Fatalf("rename during a redate: %v\n%s", err, out)
	}
}

// status lists the hidden rename temps: those an unfinished rename records (its next
// run moves them on), and one no journal records, with the mv that puts it back. An
// AppleDouble companion ("._.cull-rename-…") is never listed.
func TestStatusRenameTemps(t *testing.T) {
	dir := renameShoot(t)
	tmp := filepath.Join(dir, ".cull-rename-0a0b0c0d.M1.DNG")
	os.Rename(filepath.Join(dir, "M1.DNG"), tmp)
	os.WriteFile(filepath.Join(dir, "._.cull-rename-0a0b0c0d.M1.DNG"), []byte("x"), 0o644)
	out, _ := run(t, "status", dir)
	if !strings.Contains(out, tmp) || !strings.Contains(out, "next: mv "+shellQuote(tmp)+" "+shellQuote(filepath.Join(dir, "M1.DNG"))) ||
		strings.Contains(out, "._.cull-rename") {
		t.Fatalf("orphan:\n%s", out)
	}
	j := &journal.Rename{Pattern: "{n}", Started: time.Now(), Phase: 1, Moves: []journal.RenameMove{{Old: "M1.DNG", Tmp: filepath.Base(tmp), New: "1.DNG"}}}
	j.Save(dir)
	out, _ = run(t, "status", dir)
	if !strings.Contains(out, "1 file(s) in hidden rename temps") || !strings.Contains(out, "next: cull rename") || strings.Contains(out, "next: mv") {
		t.Fatalf("journalled:\n%s", out)
	}
}
