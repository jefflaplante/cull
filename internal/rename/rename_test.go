package rename_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/rename"
)

// Every frame, in the shoot folder and in keep/, review/ and cull/, takes its new name
// in the folder it is in, with its sidecar; the report, labels log, manifest and
// review cache follow; Verify passes; and judge makes no model calls. A label left
// under a new name by some earlier frame is cleared. restore then puts the sorted
// frames home under their new names, and judge still makes none.
func TestRenameFollowsEverything(t *testing.T) {
	dir, data, where, b := shoot(t)
	label(t, dir, "20251228_test_005.DNG", "cull", 1) // stale: no frame has this name yet
	res := runRename(t, rename.Options{Dir: dir, Pattern: pattern})
	if res.Renamed != 5 || len(res.Moves) != 5 {
		t.Fatalf("result %+v", res)
	}
	for _, m := range res.Moves {
		if !filepath.IsAbs(m.Old) || filepath.Dir(m.Old) != filepath.Dir(m.New) || !strings.HasPrefix(filepath.Base(m.Tmp), offload.RenameTempPrefix) {
			t.Errorf("move %+v", m)
		}
	}
	follows(t, dir, data, where, renamed, labelsOf(), b)
	j, err := journal.LoadRename(dir)
	if err != nil || j == nil || !j.Complete || j.Pattern != pattern {
		t.Fatalf("journal kept for --undo: %+v %v", j, err)
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("a complete rename counted as unfinished")
	}
	// The same pattern again renames nothing and keeps the journal.
	if res := runRename(t, rename.Options{Dir: dir, Pattern: pattern}); res.Renamed != 0 {
		t.Fatalf("again: %+v", res)
	}
	if j2, _ := journal.LoadRename(dir); j2 == nil || len(j2.Moves) != 5 {
		t.Fatal("a rename with nothing to do replaced the journal")
	}
	if _, err := pipeline.Restore(filepath.Join(dir, "cull-report.json"), dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	home := map[string]string{}
	for name, rel := range renamed {
		home[name] = filepath.Base(rel)
	}
	follows(t, dir, data, renamed, home, labelsOf(), b)
	// --undo finds the frames where restore put them, and names them as before.
	original := map[string]string{}
	for name := range renamed {
		original[name] = name
	}
	if res := runRename(t, rename.Options{Dir: dir, Undo: true}); res.Renamed != 5 {
		t.Fatalf("undo after restore: %+v", res)
	}
	follows(t, dir, data, home, original, labelsOf(), b)
}

// swapShoot is four offloaded frames renamed by hand so that "S{n}" swaps them in
// pairs: M1 is S2.DNG, M2 is S1.DNG (judged into cull/), M3 is S4.DNG, M4 is S3.DNG.
// The manifest records the names; S2, S4 and S3 are labelled.
func swapShoot(t *testing.T) (dir string, data map[string][]byte, where map[string]string, b *counting) {
	t.Helper()
	c, dest := t.TempDir(), t.TempDir()
	data = card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000), "M2.DNG": frame(2, 4100), "M3.DNG": frame(3, 4200), "M4.DNG": frame(4, 4300)})
	dir = offloadCard(t, c, dest)
	named := map[string]string{"M1.DNG": "S2.DNG", "M2.DNG": "S1.DNG", "M3.DNG": "S4.DNG", "M4.DNG": "S3.DNG"}
	es, err := offload.CurrentManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if err := os.Rename(filepath.Join(dir, e.Name), filepath.Join(dir, named[e.Orig])); err != nil {
			t.Fatal(err)
		}
		e.Name = named[e.Orig]
		if err := offload.AppendManifest(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	b = &counting{status: map[string]string{"S1.DNG": "missed_focus"}}
	judge(t, dir, b, func(c *pipeline.Config) { c.MoveCulled = true })
	where = map[string]string{"M1.DNG": "S2.DNG", "M2.DNG": "cull/S1.DNG", "M3.DNG": "S4.DNG", "M4.DNG": "S3.DNG"}
	for _, rel := range where {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
	}
	label(t, dir, "S2.DNG", "keep", 5)
	label(t, dir, "S4.DNG", "cull", 0)
	label(t, dir, "S3.DNG", "review", 2)
	buildSheet(t, dir)
	return dir, data, where, b
}

var swapped = map[string]string{"M1.DNG": "S1.DNG", "M2.DNG": "cull/S2.DNG", "M3.DNG": "S3.DNG", "M4.DNG": "S4.DNG"}

func swapLabels() map[string]*labels.Entry {
	return map[string]*labels.Entry{"M1.DNG": {Label: "keep", Stars: 5}, "M3.DNG": {Label: "cull"}, "M4.DNG": {Label: "review", Stars: 2}}
}

// Frames that swap names (A→B, B→A) each end up under the other's name with their own
// bytes (by hash), sidecar, report entry and label; the frame that had no label gets
// none, though its new name had one.
func TestRenameSwap(t *testing.T) {
	dir, data, where, b := swapShoot(t)
	res := runRename(t, rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
	if res.Renamed != 4 {
		t.Fatalf("result %+v", res)
	}
	follows(t, dir, data, where, swapped, swapLabels(), b)
}

// A name the plan needs that something else holds (not a frame being renamed) is
// refused before anything changes, naming it: a folder with a frame's new name, a
// foreign sidecar under a new name, a non-frame in a sort folder with a new name.
// Two frames that would get one name are refused too.
func TestRenameCollisionRefusedBeforeAnyMove(t *testing.T) {
	for _, tc := range []struct{ name, clash string }{
		{"folder", "20251228_test_004.DNG"},
		{"sidecar", "20251228_test_005.xmp"},
		{"in a sort folder", "keep/20251228_test_004.DNG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, _, _ := shoot(t)
			p := filepath.Join(dir, tc.clash)
			if strings.HasSuffix(tc.clash, ".DNG") {
				os.Mkdir(p, 0o755)
			} else {
				os.WriteFile(p, []byte("<x:xmpmeta/>"), 0o644)
			}
			before := snapshot(t, dir)
			_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
			if err == nil || !strings.Contains(err.Error(), tc.clash) {
				t.Fatalf("err %v", err)
			}
			if after := snapshot(t, dir); after != before {
				t.Fatalf("the tree changed:\n%s\n---\n%s", before, after)
			}
		})
	}
	t.Run("same name", func(t *testing.T) {
		dir := t.TempDir()
		for i, n := range []string{"a.DNG", "b.DNG"} {
			b := dngtest.Build(t, frame(int64(i), 1000+i))
			os.WriteFile(filepath.Join(dir, n), b, 0o644)
			st, _ := os.Stat(filepath.Join(dir, n))
			offload.AppendManifest(dir, offload.Entry{Orig: "M1.DNG", Name: n, Size: st.Size(), ModTime: st.ModTime(), SHA256: sum(b)})
		}
		before := snapshot(t, dir)
		_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "{orig}"})
		if err == nil || !strings.Contains(err.Error(), "a.DNG") || !strings.Contains(err.Error(), "b.DNG") || !strings.Contains(err.Error(), "M1.DNG") {
			t.Fatalf("err %v", err)
		}
		if snapshot(t, dir) != before {
			t.Fatal("the tree changed")
		}
	})
}

// A rename stopped half way through phase 1 leaves the journal unfinished: status names
// the command that finishes it, judge and redate refuse, another pattern is refused,
// and running it again finishes it, everything following. --undo then puts back every
// name, label and manifest name, and judge still makes no calls.
func TestRenameInterruptedResumesOrUndoes(t *testing.T) {
	dir, data, where, b := shoot(t)
	restore := rename.SetCrash(func(step string, i int) bool { return step == "phase1" && i == 2 })
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
	restore()
	if !errors.Is(err, rename.ErrCrash) {
		t.Fatalf("err %v", err)
	}
	which, finish, ok := journal.Incomplete(dir)
	if !ok || which != "rename" || !strings.Contains(finish, "cull rename") || !strings.Contains(finish, pattern) {
		t.Fatalf("journal %q %q %v", which, finish, ok)
	}
	out, err := cullCmd("status", dir)
	if err != nil || !strings.Contains(out, "unfinished: a rename") || !strings.Contains(out, "next: cull rename") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if out, err := cullCmd("judge", "--backend", "openai", "--model", "m", dir); err == nil || !strings.Contains(err.Error(), "cull rename") {
		t.Fatalf("judge didn't refuse: %v\n%s", err, out)
	}
	if _, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: time.Now()}); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("redate didn't refuse: %v", err)
	}
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "{orig}_x"}); err == nil || !strings.Contains(err.Error(), pattern) {
		t.Fatalf("another pattern: %v", err)
	}
	res := runRename(t, rename.Options{Dir: dir, Pattern: pattern})
	if res.Renamed != 5 {
		t.Fatalf("resumed %+v", res)
	}
	follows(t, dir, data, where, renamed, labelsOf(), b)

	res = runRename(t, rename.Options{Dir: dir, Undo: true})
	if res.Renamed != 5 {
		t.Fatalf("undo %+v", res)
	}
	follows(t, dir, data, renamed, where, labelsOf(), b)
	if j, err := journal.LoadRename(dir); j != nil || err != nil {
		t.Fatalf("journal after undo: %+v %v", j, err)
	}
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Undo: true}); err == nil {
		t.Fatal("a second undo had something to undo")
	}
}

// Wherever a rename stops (part way through either phase, after the report took its
// temp paths, or before the labels and manifest followed), running it again finishes
// it, and --undo instead puts everything back. On frames that swap names, so a step
// done twice would show.
func TestRenameStopsAnywhere(t *testing.T) {
	for _, stop := range []struct {
		step string
		i    int
	}{{"phase1", 0}, {"phase1", 2}, {"phase2", 0}, {"phase2", 2}, {"report1", 0}, {"report", 0}, {"report2", 0}, {"labels", 0}, {"manifest", 0}} {
		for _, undo := range []bool{false, true} {
			name := stop.step + "/" + string(rune('0'+stop.i))
			if undo {
				name += "/undo"
			}
			t.Run(name, func(t *testing.T) {
				dir, data, where, b := swapShoot(t)
				restore := rename.SetCrash(func(step string, i int) bool { return step == stop.step && i == stop.i })
				_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
				restore()
				if !errors.Is(err, rename.ErrCrash) {
					t.Fatalf("err %v", err)
				}
				if _, _, ok := journal.Incomplete(dir); !ok {
					t.Fatal("not unfinished")
				}
				if undo {
					runRename(t, rename.Options{Dir: dir, Undo: true})
					follows(t, dir, data, where, where, swapLabelsAt(where), b)
					if j, _ := journal.LoadRename(dir); j != nil {
						t.Fatalf("journal left: %+v", j)
					}
					return
				}
				runRename(t, rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
				follows(t, dir, data, where, swapped, swapLabels(), b)
			})
		}
	}
}

// swapLabelsAt is swapShoot's labels by camera name (they don't depend on where).
func swapLabelsAt(map[string]string) map[string]*labels.Entry {
	return map[string]*labels.Entry{"M1.DNG": {Label: "keep", Stars: 5}, "M3.DNG": {Label: "cull"}, "M4.DNG": {Label: "review", Stars: 2}}
}

// An undo stopped part way is finished by running --undo again.
func TestRenameUndoStopped(t *testing.T) {
	for _, step := range []string{"phase1", "phase2", "report", "manifest"} {
		t.Run(step, func(t *testing.T) {
			dir, data, where, b := swapShoot(t)
			runRename(t, rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
			stopAt := 0
			if step == "phase1" || step == "phase2" {
				stopAt = 1
			}
			restore := rename.SetCrash(func(s string, i int) bool { return s == step && i == stopAt })
			_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Undo: true})
			restore()
			if !errors.Is(err, rename.ErrCrash) {
				t.Fatalf("err %v", err)
			}
			if _, finish, ok := journal.Incomplete(dir); !ok || !strings.Contains(finish, "--undo") {
				t.Fatalf("unfinished undo: %q %v", finish, ok)
			}
			if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true}); err == nil || !strings.Contains(err.Error(), "--undo") {
				t.Fatalf("a rename during an unfinished undo: %v", err)
			}
			runRename(t, rename.Options{Dir: dir, Undo: true})
			follows(t, dir, data, swapped, where, swapLabelsAt(where), b)
		})
	}
}

// --dry-run prints old → new (relative paths) and writes nothing.
func TestRenameDryRun(t *testing.T) {
	dir, _, _, _ := shoot(t)
	before := snapshot(t, dir)
	var lines []string
	res, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern, DryRun: true, UI: notes(&lines)})
	if err != nil || len(res.Moves) != 5 || res.Renamed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"cull/M1.DNG → cull/20251228_test_001.DNG", "M4.DNG → 20251228_test_004.DNG"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in:\n%s", want, joined)
		}
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("dry run changed the tree:\n%s\n---\n%s", before, after)
	}
}
