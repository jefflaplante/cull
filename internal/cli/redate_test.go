package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
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
	if err != nil || !strings.Contains(out, "next: cull redate "+dir+" --date 2026-10-04") || !strings.Contains(out, "unfinished") {
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
