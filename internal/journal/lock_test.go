package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared holders share; an exclusive holder excludes everyone, and a refusal names the
// holder. The lock file stays (removing it would split later holders).
func TestLock(t *testing.T) {
	dir := t.TempDir()
	r1, _, err := Lock(dir, false, "judge")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Lock(dir, false, "review")
	if err != nil {
		t.Fatalf("two shared holders: %v", err)
	}
	if _, _, err := Lock(dir, true, "rename"); err == nil || !strings.Contains(err.Error(), "cull ") {
		t.Fatalf("exclusive while shared: %v", err)
	}
	r1()
	r2()
	rx, _, err := Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Lock(dir, false, "judge")
	if err == nil || !strings.Contains(err.Error(), "cull rename") || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("shared while exclusive: %v", err)
	}
	rx()
	if _, err := os.Stat(filepath.Join(dir, LockName)); err != nil {
		t.Fatal(err)
	}
	if r, _, err := Lock(dir, true, "redate"); err != nil {
		t.Fatal(err)
	} else {
		r()
	}
}

// An unfinished redate and an unfinished rename in one folder, or below it: both are
// reported.
func TestIncompleteBelowEveryKind(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "day1")
	os.Mkdir(sub, 0o755)
	(&Redate{Target: "2026-10-04T12:00:00"}).Save(dir)
	(&Rename{Pattern: "{n}", Phase: 1}).Save(dir)
	(&Rename{Pattern: "{n:2}", Phase: 1}).Save(sub)
	got := IncompleteBelow(dir)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	kinds := map[string]bool{}
	for _, p := range got {
		kinds[p.Folder+" "+p.Which] = true
	}
	if !kinds[dir+" redate"] || !kinds[dir+" rename"] || !kinds[sub+" rename"] {
		t.Fatalf("%+v", got)
	}
}
