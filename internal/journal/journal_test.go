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

// The rename journal round-trips, names the command that finishes it (with -r and -o
// when it ran with them), and an undo journal names --undo.
func TestRenameJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 Smith wedding")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if j, err := LoadRename(dir); j != nil || err != nil {
		t.Fatalf("none: %+v %v", j, err)
	}
	j := &Rename{Pattern: "{date}_{n:4}", Started: time.Now(), Phase: 1, Moves: []RenameMove{
		{Old: "M1.DNG", Tmp: ".cull-rename-0123abcd.M1.DNG", New: "20251228_0001.DNG", Size: 3, ModTime: time.Unix(5, 6)},
	}}
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRename(dir)
	if err != nil || got == nil || len(got.Moves) != 1 || got.Phase != 1 {
		t.Fatalf("load %+v %v", got, err)
	}
	// Times compare with Equal: JSON brings a time back in UTC or Local depending on
	// the machine's zone, so == on the struct fails under TZ=UTC (the release runner).
	gm, wm := got.Moves[0], j.Moves[0]
	if !gm.ModTime.Equal(wm.ModTime) {
		t.Fatalf("mod time %v, want %v", gm.ModTime, wm.ModTime)
	}
	gm.ModTime, wm.ModTime = time.Time{}, time.Time{}
	if gm != wm {
		t.Fatalf("load %+v, want %+v", gm, wm)
	}
	which, finish, ok := Incomplete(dir)
	want := "cull rename '" + dir + "' '{date}_{n:4}' (or cull rename --undo '" + dir + "')"
	if !ok || which != "rename" || finish != want {
		t.Fatalf("%q %q %v", which, finish, ok)
	}
	j.Recursive, j.Report = true, "/r/x.json"
	if f := j.Finish(dir); f != "cull rename -r -o /r/x.json '"+dir+"' '{date}_{n:4}' (or cull rename --undo -r -o /r/x.json '"+dir+"')" {
		t.Fatalf("finish %q", f)
	}
	j.Recursive, j.Report = false, ""
	j.Undo = true
	j.Save(dir)
	if _, finish, ok := Incomplete(dir); !ok || finish != "cull rename --undo '"+dir+"'" {
		t.Fatalf("undo: %q %v", finish, ok)
	}
	j.Complete = true
	j.Save(dir)
	if _, _, ok := Incomplete(dir); ok {
		t.Fatal("complete journal unfinished")
	}
	if err := RemoveRename(dir); err != nil {
		t.Fatal(err)
	}
	if j, err := LoadRename(dir); j != nil || err != nil {
		t.Fatalf("after remove: %+v %v", j, err)
	}
}
