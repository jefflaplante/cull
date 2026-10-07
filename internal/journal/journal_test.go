package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIncompleteNone(t *testing.T) {
	if which, finish, ok := Incomplete(t.TempDir()); ok {
		t.Fatalf("empty folder: %q %q", which, finish)
	}
}

// An unfinished redate names the command that finishes it, quoted for the shell, with
// --time only when it isn't the default.
func TestIncompleteRedate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 Smith wedding")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	j := &Redate{Target: "2026-10-04T12:00:00", Started: time.Now()}
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	which, finish, ok := Incomplete(dir)
	if !ok || which != "redate" || finish != "cull redate '"+dir+"' --date 2026-10-04" {
		t.Fatalf("%q %q %v", which, finish, ok)
	}
	j.Target = "2026-10-04T09:30:00"
	j.Save(dir)
	if _, finish, _ := Incomplete(dir); !strings.HasSuffix(finish, "--date 2026-10-04 --time 09:30:00") {
		t.Fatalf("finish %q", finish)
	}
	got, err := LoadRedate(dir)
	if err != nil || got == nil || got.Target != j.Target {
		t.Fatalf("load %+v %v", got, err)
	}
	if err := RemoveRedate(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Incomplete(dir); ok {
		t.Fatal("removed journal still incomplete")
	}
	if got, err := LoadRedate(dir); got != nil || err != nil {
		t.Fatalf("load after remove %+v %v", got, err)
	}
}

// A journal that can't be read counts as unfinished: refusing is the safe side.
func TestIncompleteUnreadable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, RedateName), []byte("{torn"), 0o644)
	if which, _, ok := Incomplete(dir); !ok || which != "redate" {
		t.Fatalf("%q %v", which, ok)
	}
}

// A rename journal marked complete is kept for --undo and is not unfinished.
func TestIncompleteRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, RenameName)
	os.WriteFile(p, []byte(`{"pattern":"{n:4}","complete":false}`), 0o644)
	which, finish, ok := Incomplete(dir)
	if !ok || which != "rename" || !strings.Contains(finish, "cull rename") || !strings.Contains(finish, "{n:4}") {
		t.Fatalf("%q %q %v", which, finish, ok)
	}
	os.WriteFile(p, []byte(`{"pattern":"{n:4}","complete":true}`), 0o644)
	if _, _, ok := Incomplete(dir); ok {
		t.Fatal("a complete rename journal counted as unfinished")
	}
}
