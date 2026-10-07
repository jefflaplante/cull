package rename_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/rename"
)

// loose writes frames straight into dir (copied with Finder: no manifest, no report).
func loose(t *testing.T, dir string, files map[string]dngtest.Fixture) map[string][]byte {
	t.Helper()
	data := map[string][]byte{}
	for name, fx := range files {
		b := dngtest.Build(t, fx)
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, cardTime, cardTime)
		data[name] = b
	}
	return data
}

// A sort folder given as the folder is refused, naming the shoot folder; so is -r on
// a folder holding another shoot folder. Nothing changes.
func TestRenameRefusesSortFolderAndParent(t *testing.T) {
	dir, _, _, _ := shoot(t)
	before := snapshot(t, dir)
	_, err := rename.Run(context.Background(), rename.Options{Dir: filepath.Join(dir, "keep"), Pattern: pattern})
	if err == nil || !strings.Contains(err.Error(), "sort folder") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("sort folder: %v", err)
	}
	parent := filepath.Dir(dir)
	_, err = rename.Run(context.Background(), rename.Options{Dir: parent, Pattern: pattern, Recursive: true})
	if err == nil || !strings.Contains(err.Error(), "shoot folder of its own") {
		t.Fatalf("-r parent: %v", err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("the tree changed")
	}
}

// -r renames frames in plain subfolders too, each in its folder, counting across them.
func TestRenameRecursive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	data := loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000), "day1/B1.DNG": frame(2, 1100), "day2/C1.DNG": frame(3, 1200)})
	res := runRename(t, rename.Options{Dir: dir, Pattern: "{name}_{n}", Recursive: true})
	if res.Renamed != 3 {
		t.Fatalf("%+v", res)
	}
	for old, rel := range map[string]string{"A1.DNG": "trip_1.DNG", "day1/B1.DNG": "day1/trip_2.DNG", "day2/C1.DNG": "day2/trip_3.DNG"} {
		if sum(read(t, filepath.Join(dir, rel))) != sum(data[old]) {
			t.Errorf("%s isn't %s", rel, old)
		}
	}
	// --undo needs the same -r.
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Undo: true}); err == nil || !strings.Contains(err.Error(), "-r") {
		t.Fatalf("undo without -r: %v", err)
	}
	runRename(t, rename.Options{Dir: dir, Undo: true, Recursive: true})
	for old := range data {
		if _, err := os.Stat(filepath.Join(dir, old)); err != nil {
			t.Error(err)
		}
	}
	// No manifest, report or labels log appeared.
	for _, n := range []string{offload.ManifestName, "cull-report.json", "cull-labels.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s created", n)
		}
	}
}

// {date} is the date the manifest records as set (redate, offload --set-date) over
// EXIF; a frame no manifest records is ordered by its name.
func TestRenameDateSetAndOrder(t *testing.T) {
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, map[string]dngtest.Fixture{"M2.DNG": frame(2, 1000), "M1.DNG": frame(1, 1100)})
	dir := offloadCard(t, c, dest)
	es, _ := offload.CurrentManifest(dir)
	for _, e := range es {
		if e.Orig == "M2.DNG" {
			e.DatesSet = "2026-10-04T12:00:00"
			offload.AppendManifest(dir, e)
		}
	}
	loose(t, dir, map[string]dngtest.Fixture{"A0.DNG": frame(3, 1200)}) // unrecorded: sorts before M1 by name
	runRename(t, rename.Options{Dir: dir, Pattern: "{date}_{n}"})
	for _, n := range []string{"20251228_1.DNG", "20251228_2.DNG", "20261004_3.DNG"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Error(err)
		}
	}
}

// A move the re-run can't be sure of is left as it is, named, with what to do, and
// the journal stays unfinished; once dealt with, the re-run finishes.
func TestRenameHeld(t *testing.T) {
	t.Run("a stranger at a new name", func(t *testing.T) {
		dir, data, where, b := shoot(t)
		restore := rename.SetCrash(func(step string, i int) bool { return step == "phase1" && i == 4 })
		rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
		restore()
		stranger := filepath.Join(dir, "20251228_test_005.DNG")
		os.WriteFile(stranger, []byte("not a frame"), 0o644)
		res, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
		if err == nil || len(res.Held) != 1 || !strings.Contains(res.Held[0], "20251228_test_005.DNG") || !strings.Contains(res.Held[0], "aside") {
			t.Fatalf("%v %+v", err, res.Held)
		}
		if got := string(read(t, stranger)); got != "not a frame" {
			t.Fatal("the stranger was replaced")
		}
		if _, _, ok := journal.Incomplete(dir); !ok {
			t.Fatal("journal complete")
		}
		if out, _ := cullCmd("status", dir); !strings.Contains(out, "hidden rename temps") || !strings.Contains(out, "next: cull rename") {
			t.Fatalf("status:\n%s", out)
		}
		os.Rename(stranger, filepath.Join(t.TempDir(), "aside"))
		runRename(t, rename.Options{Dir: dir, Pattern: pattern})
		follows(t, dir, data, where, renamed, labelsOf(), b)
	})
	t.Run("old name and temp both there", func(t *testing.T) {
		dir, data, where, b := shoot(t)
		restore := rename.SetCrash(func(step string, i int) bool { return step == "phase1" && i == 0 })
		rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
		restore()
		j, _ := journal.LoadRename(dir)
		m := j.Moves[0]
		copyFile(t, filepath.Join(dir, m.Tmp), filepath.Join(dir, m.Old)) // a second file under the old name
		res, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
		if err == nil || len(res.Held) != 1 || !strings.Contains(res.Held[0], "aren't one file") {
			t.Fatalf("%v %+v", err, res.Held)
		}
		if _, err := os.Stat(filepath.Join(dir, m.Tmp)); err != nil {
			t.Fatal("the temp is gone")
		}
		os.Remove(filepath.Join(dir, m.Old)) // the user removes the duplicate
		runRename(t, rename.Options{Dir: dir, Pattern: pattern})
		follows(t, dir, data, where, renamed, labelsOf(), b)
	})
}

// TestRenameHeldStranger: after a crash in phase 2, a file that isn't the frame (another
// size) sits under a frame's new name and its temp is gone: never taken for the frame.
func TestRenameHeldStranger(t *testing.T) {
	dir, _, _, _ := shoot(t)
	restore := rename.SetCrash(func(step string, i int) bool { return step == "phase2" && i == 0 })
	rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
	restore()
	j, _ := journal.LoadRename(dir)
	m := j.Moves[0]
	aside := filepath.Join(t.TempDir(), "frame")
	os.Rename(filepath.Join(dir, m.New), aside)
	os.WriteFile(filepath.Join(dir, m.New), []byte("another file"), 0o644)
	res, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
	if err == nil || len(res.Held) != 1 || !strings.Contains(res.Held[0], "isn't the one the rename recorded") {
		t.Fatalf("%v %+v", err, res.Held)
	}
	if _, _, ok := journal.Incomplete(dir); !ok {
		t.Fatal("journal complete")
	}
	rep := string(read(t, filepath.Join(dir, "cull-report.json")))
	if strings.Contains(rep, filepath.Join(dir, m.New)) {
		t.Fatal("the report follows a file that isn't the frame")
	}
	os.Remove(filepath.Join(dir, m.New))
	os.Rename(aside, filepath.Join(dir, m.New))
	runRename(t, rename.Options{Dir: dir, Pattern: pattern})
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b := read(t, from)
	st, _ := os.Stat(from)
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(to, st.ModTime(), st.ModTime())
}

// Frames named "_DSC…" (Nikon, Sony; Pentax "_IGP", Canon "_MG_"): their temps never
// start "._", so AppleDouble companions of the temps ("._.cull-rename-…") are never
// taken for temps, and an interrupted rename finishes.
func TestRenameUnderscoreNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	data := loose(t, dir, map[string]dngtest.Fixture{"_DSC0001.DNG": frame(1, 1000), "_DSC0002.DNG": frame(2, 1100), "_DSC0003.DNG": frame(3, 1200)})
	restore := rename.SetCrash(func(step string, i int) bool { return step == "phase1" && i == 1 })
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"})
	restore()
	if !errors.Is(err, rename.ErrCrash) {
		t.Fatal(err)
	}
	j, _ := journal.LoadRename(dir)
	var companions []string
	for _, m := range j.Moves {
		if strings.HasPrefix(filepath.Base(m.Tmp), "._") {
			t.Fatalf("temp %s looks like an AppleDouble companion", m.Tmp)
		}
		c := filepath.Join(dir, "._"+filepath.Base(m.Tmp))
		os.WriteFile(c, []byte("appledouble"), 0o644)
		companions = append(companions, c)
	}
	runRename(t, rename.Options{Dir: dir, Pattern: "x{n}"})
	for i, old := range []string{"_DSC0001.DNG", "_DSC0002.DNG", "_DSC0003.DNG"} {
		if sum(read(t, filepath.Join(dir, "x"+string(rune('1'+i))+".DNG"))) != sum(data[old]) {
			t.Errorf("%s not renamed", old)
		}
	}
	for _, c := range companions {
		if got := string(read(t, c)); got != "appledouble" {
			t.Errorf("companion %s touched", c)
		}
	}
}

// The plan refuses a shoot whose frames share a name across its sort folders.
func TestRenameDuplicateNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	loose(t, dir, map[string]dngtest.Fixture{"M1.DNG": frame(1, 1000), "keep/M1.DNG": frame(2, 1100)})
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"}); err == nil || !strings.Contains(err.Error(), "same name") {
		t.Fatalf("%v", err)
	}
}

// The journal is on the media before the first move; each phase's moves are flushed
// (their folders) before the journal moves on; the report, labels log and manifest are
// flushed before the journal says complete.
func TestRenameDurabilityOrder(t *testing.T) {
	dir, _, _, _ := shoot(t)
	var ev []string
	restore := rename.SetTrace(func(e string) { ev = append(ev, e) })
	runRename(t, rename.Options{Dir: dir, Pattern: pattern})
	restore()
	at := func(pred func(string) bool, from int) int {
		for i := from; i < len(ev); i++ {
			if pred(ev[i]) {
				return i
			}
		}
		return -1
	}
	is := func(s string) func(string) bool { return func(e string) bool { return e == s } }
	has := func(s string) func(string) bool { return func(e string) bool { return strings.HasPrefix(e, s) } }
	jflush := "flush " + filepath.Join(dir, journal.RenameName)
	first := at(has("move "), 0)
	if j := at(is("journal phase=1 report= complete=false"), 0); j < 0 || first < 0 || at(is(jflush), j) < 0 || at(is(jflush), j) > first {
		t.Fatalf("journal not flushed before the first move:\n%s", strings.Join(ev, "\n"))
	}
	p2 := at(is("journal phase=2 report= complete=false"), 0)
	lastP1 := -1
	for i, e := range ev[:p2] {
		if strings.HasPrefix(e, "move ") {
			lastP1 = i
		}
	}
	for _, d := range []string{dir, filepath.Join(dir, "keep"), filepath.Join(dir, "review"), filepath.Join(dir, "cull")} {
		if f := at(is("flush "+d), lastP1); f < 0 || f > p2 {
			t.Errorf("%s not flushed between phase 1 and the journal's phase 2:\n%s", d, strings.Join(ev, "\n"))
		}
	}
	done := at(has("journal phase=2 report=new complete=true"), 0)
	for _, need := range []string{"flush " + filepath.Join(dir, "cull-report.json"), "flush " + filepath.Join(dir, "cull-labels.jsonl"), "flush " + filepath.Join(dir, offload.ManifestName)} {
		if f := at(is(need), p2); f < 0 || f > done {
			t.Errorf("%s not before the journal is complete:\n%s", need, strings.Join(ev, "\n"))
		}
	}
	lastMove := -1
	for i, e := range ev {
		if strings.HasPrefix(e, "move ") {
			lastMove = i
		}
	}
	if f := at(is("flush "+dir), lastMove); f < 0 || f > at(has("report save"), 0) {
		t.Errorf("phase 2 not flushed before the bookkeeping:\n%s", strings.Join(ev, "\n"))
	}
}
