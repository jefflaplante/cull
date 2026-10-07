package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
)

// While a redate is unfinished, judge, decide and review refuse, naming the command
// that finishes it, and status points at it.
func TestUnfinishedRedateGuards(t *testing.T) {
	dir := statusShoot(t)
	j := &journal.Redate{Target: "2026-10-04T12:00:00", Started: time.Now()}
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"judge", "--backend", "openai", "--model", "m", dir},
		{"decide", dir},
		{"review", "--static", dir},
	} {
		out, err := run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "cull redate "+dir+" --date 2026-10-04") {
			t.Errorf("%v: err %v\n%s", args[0], err, out)
		}
	}
	out, err := run(t, "status", dir)
	if err != nil || !strings.Contains(out, "next: cull redate "+dir+" --date 2026-10-04") || !strings.Contains(out, "unfinished") || !strings.Contains(out, "restore, scan, tag, rank, import-labels, offload, redate and rename refuse") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	// And without a report.
	bare := t.TempDir()
	j.Save(bare)
	if out, _ := run(t, "status", bare); !strings.Contains(out, "next: cull redate") {
		t.Fatalf("status without a report:\n%s", out)
	}
}

func redateShoot(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", Payload: []byte("raw")})
	if err := os.WriteFile(filepath.Join(dir, "M1.DNG"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, b
}

func TestRedateCommand(t *testing.T) {
	dir, orig := redateShoot(t)
	p := filepath.Join(dir, "M1.DNG")
	out, err := run(t, "redate", "--dry-run", "--date", "2026-10-04", "--time", "09:30:00", dir)
	if err != nil || !strings.Contains(out, "M1.DNG: 2 fields → 2026-10-04 09:30:00") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, orig) {
		t.Fatal("dry run changed the file")
	}
	out, err = run(t, "redate", "--date", "2026-10-04", "--time", "09:30:00", dir)
	if err != nil || !strings.Contains(out, "patched 1") || !strings.Contains(out, "Capture One or Lightroom") {
		t.Fatalf("%v\n%s", err, out)
	}
	st, _ := os.Stat(p)
	if want := time.Date(2026, 10, 4, 9, 30, 0, 0, time.Local); !st.ModTime().Equal(want) {
		t.Fatalf("mtime %v", st.ModTime())
	}
	out, err = run(t, "redate", "--date", "2026-10-04", "--time", "09:30:00", dir)
	if err != nil || !strings.Contains(out, "already set 1") {
		t.Fatalf("again: %v\n%s", err, out)
	}
}

func TestRedateFlagErrors(t *testing.T) {
	dir, _ := redateShoot(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{}, "--date"},
		{[]string{"--date", "2026-13-04"}, "--date"},
		{[]string{"--date", "2026-10-04", "--time", "9:30"}, "--time"},
	} {
		out, err := run(t, append(append([]string{"redate"}, c.args...), dir)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err %v\n%s", c.args, err, out)
		}
	}
}

// A refused file fails the command, so a script notices.
func TestRedateRefusalFails(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "X.DNG"), []byte("not a tiff"), 0o644)
	out, err := run(t, "redate", "--date", "2026-10-04", dir)
	if err == nil || !strings.Contains(out, "refused 1") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDateNotInDotfile(t *testing.T) {
	if !notInDotfile["date"] {
		t.Fatal("date settable from the dotfile")
	}
}

// judge -r on a parent refuses while a shoot folder below it has an unfinished
// redate; restore refuses while the folder's own is unfinished.
func TestUnfinishedRedateGuardsRecursiveAndRestore(t *testing.T) {
	parent := t.TempDir()
	shoot := filepath.Join(parent, "2026-10-04 Test")
	os.Mkdir(shoot, 0o755)
	tinyDNG(t, filepath.Join(shoot, "L1.DNG"))
	j := &journal.Redate{Target: "2026-10-04T12:00:00", Started: time.Now()}
	if err := j.Save(shoot); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "judge", "-r", "--backend", "openai", "--model", "m", parent)
	if err == nil || !strings.Contains(err.Error(), "cull redate") || !strings.Contains(err.Error(), "2026-10-04 Test") {
		t.Fatalf("judge -r: %v\n%s", err, out)
	}
	out, err = run(t, "restore", shoot)
	if err == nil || !strings.Contains(err.Error(), "cull redate") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
}

// An interrupted replacement: the closing lines say so (not "refused, left as they
// are") and name the command that finishes it; status points at that command, not at a
// manual mv.
func TestRedateInterruptedReplacement(t *testing.T) {
	dir, _ := redateShoot(t)
	restore := offload.SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	out, err := run(t, "redate", "--date", "2026-10-04", dir)
	restore()
	if err == nil || !strings.Contains(out, "1 replacement(s) interrupted: run redate again to restore them") ||
		!strings.Contains(out, "unfinished: run cull redate "+dir+" --date 2026-10-04 to finish it") || strings.Contains(out, "left as they are") {
		t.Fatalf("%v\n%s", err, out)
	}
	out, _ = run(t, "status", dir)
	if !strings.Contains(out, "next: cull redate "+dir+" --date 2026-10-04") || strings.Contains(out, "next: mv") {
		t.Fatalf("status:\n%s", out)
	}
}

// status lists a temp beside its frame too (redate keeps it when the frame isn't what
// it recorded).
func TestStatusTempBesideFrame(t *testing.T) {
	dir, _ := redateShoot(t)
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, []byte("x"), 0o644)
	out, err := run(t, "status", dir)
	if err != nil || !strings.Contains(out, tmp) {
		t.Fatalf("status: %v\n%s", err, out)
	}
}

// A frame named "_IGP0001.DNG" (Pentax; Nikon/Sony "_DSC", Canon "_MG_") interrupted
// mid-replacement: status names it and its hidden temp (once taken for an AppleDouble
// companion and never shown), and the next redate restores it.
func TestStatusUnderscoreInterrupted(t *testing.T) {
	dir := t.TempDir()
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", Payload: []byte("raw")})
	p := filepath.Join(dir, "_IGP0001.DNG")
	os.WriteFile(p, b, 0o644)
	restore := offload.SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	out, err := run(t, "redate", "--date", "2026-10-04", dir)
	restore()
	if err == nil {
		t.Fatalf("redate: no error\n%s", out)
	}
	out, _ = run(t, "status", dir)
	if !strings.Contains(out, "missing: _IGP0001.DNG; the next redate restores it") || !strings.Contains(out, ".cull-redate-") ||
		!strings.Contains(out, "next: cull redate "+dir+" --date 2026-10-04") || !strings.Contains(out, ": 0 DNGs") {
		t.Fatalf("status:\n%s", out)
	}
	if out, err := run(t, "redate", "--date", "2026-10-04", dir); err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("_IGP0001.DNG not restored")
	}
}

// An AppleDouble companion of a redate temp ("._.cull-redate-…") is never listed as a
// temp, nor counted as a DNG.
func TestStatusIgnoresAppleDoubleOfTemp(t *testing.T) {
	dir, _ := redateShoot(t)
	os.WriteFile(filepath.Join(dir, "._.cull-redate-deadbeef.M9.DNG"), []byte("x"), 0o644)
	out, err := run(t, "status", dir)
	if err != nil || strings.Contains(out, "cull-redate-deadbeef") || strings.Contains(out, "next: mv") {
		t.Fatalf("status: %v\n%s", err, out)
	}
}

// -r: an interrupted replacement in a plain subfolder; status -r lists it and the
// redate that restores it, which then finishes.
func TestStatusRecursiveInterrupted(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "day1")
	os.Mkdir(sub, 0o755)
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", Payload: []byte("raw")})
	os.WriteFile(filepath.Join(sub, "_DSC0001.DNG"), b, 0o644)
	restore := offload.SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	run(t, "redate", "-r", "--date", "2026-10-04", parent)
	restore()
	out, _ := run(t, "status", "-r", parent)
	if !strings.Contains(out, "day1/_DSC0001.DNG; the next redate restores it") || !strings.Contains(out, "next: cull redate") {
		t.Fatalf("status -r:\n%s", out)
	}
	if out, err := run(t, "redate", "-r", "--date", "2026-10-04", parent); err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
}

// An orphan in a subfolder with -r: the CLI error names cull status -r, which lists it.
func TestOrphanRecursiveCLI(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "day1")
	os.Mkdir(sub, 0o755)
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", Payload: []byte("raw")})
	os.WriteFile(filepath.Join(sub, "M1.DNG"), b, 0o644)
	os.WriteFile(filepath.Join(sub, ".cull-redate-deadbeef._M2.DNG"), b, 0o644)
	out, err := run(t, "redate", "-r", "--date", "2026-10-04", parent)
	if err == nil || !strings.Contains(err.Error(), "cull status -r") {
		t.Fatalf("%v\n%s", err, out)
	}
	out, _ = run(t, "status", "-r", parent)
	if !strings.Contains(out, ".cull-redate-deadbeef._M2.DNG") || !strings.Contains(out, "next: mv -n") || !strings.Contains(out, "day1/_M2.DNG") {
		t.Fatalf("status -r:\n%s", out)
	}
}
