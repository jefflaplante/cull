package rename_test

// Fix round 1 (integrity review of c925132): adapted from the reviewer's adversarial
// tests (adv_test.go, adv2_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/rename"
)

// replanCopies re-plans an offload of card c into dest and returns how many files it
// would copy; a refusal fails the test.
func replanCopies(t *testing.T, c, dest string, o offload.Options) int {
	t.Helper()
	o.Sources, o.Dest, o.Name, o.Date = []string{c}, dest, "test", "2026-10-02"
	p, err := offload.MakePlan(o)
	if err != nil {
		t.Fatalf("re-plan refused: %v", err)
	}
	n := 0
	for _, f := range p.Files {
		if f.Skip == "" {
			t.Logf("%s would be copied again as %s", f.Src, f.Name)
			n++
		}
	}
	return n
}

// CRITICAL: re-running offload on the same card after a rename copies nothing, with
// camera names (plain and --checksum), with --rename, and when the new names are other
// frames' camera names; and the run says "safe to format".
func TestAdvOffloadRerunAfterRename(t *testing.T) {
	for _, tc := range []struct {
		name, pattern string
		o             offload.Options
		files         []string
	}{
		{"camera-names", "x_{n:3}", offload.Options{}, []string{"M1.DNG", "M2.DNG"}},
		{"camera-names-checksum", "x_{n:3}", offload.Options{Checksum: true}, []string{"M1.DNG", "M2.DNG"}},
		{"rename-pattern", "x_{n:3}", offload.Options{Rename: "{orig}"}, []string{"M1.DNG", "M2.DNG"}},
		// A → M1, M1 → M2: a new name is another frame's camera name.
		{"names-of-others", "M{n}", offload.Options{}, []string{"A.DNG", "M1.DNG"}},
		{"names-of-others-checksum", "M{n}", offload.Options{Checksum: true}, []string{"A.DNG", "M1.DNG"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dest := t.TempDir(), t.TempDir()
			fx := map[string]dngtest.Fixture{}
			for i, n := range tc.files {
				fx[n] = frame(int64(i+1), 4000+100*i)
			}
			card(t, c, fx)
			o := tc.o
			o.Sources, o.Dest, o.Name, o.Date = []string{c}, dest, "test", "2026-10-02"
			p, err := offload.MakePlan(o)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := offload.Run(context.Background(), p, nil); err != nil || !res.Safe {
				t.Fatalf("%v %+v", err, res)
			}
			dir := p.Dests[0]
			runRename(t, rename.Options{Dir: dir, Pattern: tc.pattern})
			if n := replanCopies(t, c, dest, tc.o); n != 0 {
				t.Fatalf("%d frame(s) would be copied again", n)
			}
			p, _ = offload.MakePlan(o)
			res, err := offload.Run(context.Background(), p, nil)
			if err != nil || !res.Safe || res.Copied != 0 {
				t.Fatalf("re-run: %v %+v", err, res)
			}
		})
	}
}

func lookJPEG(kind int) []byte {
	img := image.NewGray(image.Rect(0, 0, 1600, 1067))
	for y := 0; y < 1067; y++ {
		for x := 0; x < 1600; x++ {
			var v int
			switch kind {
			case 0:
				v = 40 + x*160/1600
			case 1:
				v = 40 + y*160/1067
			default:
				if (x/400+y/267)%2 == 0 {
					v = 40
				} else {
					v = 200
				}
			}
			img.Pix[y*1600+x] = uint8(v)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

// ranker answers rank calls (best first, in the order shown) and counts them.
type ranker struct {
	counting
	ranks int32
}

func (r *ranker) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if req.SchemaName == "ranking" {
		atomic.AddInt32(&r.calls, 1)
		atomic.AddInt32(&r.ranks, 1)
		n := 0
		for _, p := range req.Parts {
			if strings.HasSuffix(p.Text, ": full frame") {
				n++
			}
		}
		var es []string
		for i := 1; i <= n; i++ {
			es = append(es, fmt.Sprintf(`{"frame":%d,"strength":"s","weakness":"w"}`, i))
		}
		return &llm.Response{JSON: json.RawMessage(`{"summary":"x","ranking":[` + strings.Join(es, ",") + `]}`)}, nil
	}
	return r.counting.Call(ctx, req)
}

// rankedShoot is 12 frames with equal capture times, judged and ranked in sets:
// offloaded (the manifest records their card names), or copied in loose (none does).
func rankedShoot(t *testing.T, offloaded bool) (string, *ranker, pipeline.Config) {
	t.Helper()
	// Sets {L01, L02} and {L11, L12}; with "{n}" the order becomes s_1 s_10 s_11 s_12 s_2 …
	looks := []int{0, 0, 2, 1, 2, 1, 2, 1, 2, 0, 1, 1}
	files := map[string]dngtest.Fixture{}
	for i, k := range looks {
		fx := frame(int64(i+1), 4000+100*i)
		fx.Preview = lookJPEG(k)
		files[fmt.Sprintf("L%02d.DNG", i+1)] = fx
	}
	var dir string
	if offloaded {
		c, dest := t.TempDir(), t.TempDir()
		card(t, c, files)
		dir = offloadCard(t, c, dest)
	} else {
		dir = filepath.Join(t.TempDir(), "2026-10-02 test")
		loose(t, dir, files)
	}
	b := &ranker{}
	cfg := judgeCfg(dir)
	cfg.Resume, cfg.Rank = true, true
	cfg.Seq = group.Options{Gap: time.Hour, MaxLook: group.DefaultLook}
	cfg.Policy.KeepBest = 2
	if _, _, err := pipeline.Run(context.Background(), cfg, b); err != nil {
		t.Fatal(err)
	}
	if b.ranks == 0 {
		t.Fatal("nothing ranked")
	}
	return dir, b, cfg
}

func calls(t *testing.T, b *ranker, cfg pipeline.Config) int32 {
	t.Helper()
	before := atomic.LoadInt32(&b.calls)
	if _, _, err := pipeline.Run(context.Background(), cfg, b); err != nil {
		t.Fatal(err)
	}
	return atomic.LoadInt32(&b.calls) - before
}

// IMPORTANT 1: with ranking on, a rename that keeps the frames' order (`{n:2}`) costs
// judge no calls. Frames a manifest records keep their card names' camera order
// whatever they're called, so even `{n}` (12 frames: 10 sorts before 2) costs none.
// Frames no manifest records are ordered by their names: there `{n}` changes the
// order and is refused, naming the cause, in a dry run too; with Reorder it goes
// ahead and says how many sets change.
func TestAdvRankReorder(t *testing.T) {
	for _, offloaded := range []bool{true, false} {
		t.Run(fmt.Sprintf("padded offloaded=%v", offloaded), func(t *testing.T) {
			dir, b, cfg := rankedShoot(t, offloaded)
			runRename(t, rename.Options{Dir: dir, Pattern: "s_{n:2}"})
			if n := calls(t, b, cfg); n != 0 {
				t.Fatalf("judge (ranking on) after the rename made %d calls", n)
			}
		})
	}
	t.Run("unpadded offloaded", func(t *testing.T) {
		dir, b, cfg := rankedShoot(t, true)
		runRename(t, rename.Options{Dir: dir, Pattern: "s_{n}"})
		if n := calls(t, b, cfg); n != 0 {
			t.Fatalf("judge (ranking on) after the rename made %d calls", n)
		}
	})
	t.Run("unpadded loose", func(t *testing.T) {
		dir, _, _ := rankedShoot(t, false)
		before := snapshot(t, dir)
		for _, dry := range []bool{true, false} {
			_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "s_{n}", DryRun: dry})
			if err == nil || !strings.Contains(err.Error(), "{n:") || !strings.Contains(err.Error(), "--reorder") {
				t.Fatalf("dry %v: %v", dry, err)
			}
		}
		if snapshot(t, dir) != before {
			t.Fatal("the tree changed")
		}
		var lines []string
		res, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "s_{n}", Reorder: true, UI: notes(&lines)})
		if err != nil || res.Renamed != 12 {
			t.Fatalf("%v %+v", err, res)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "set(s)") {
			t.Fatalf("no note on the sets:\n%s", strings.Join(lines, "\n"))
		}
	})
}

// IMPORTANT 2: names the volume can't hold (over 255 bytes, the new name or a temp) are
// refused before anything moves, in a dry run too.
func TestAdvNameTooLong(t *testing.T) {
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000), "M2.DNG": frame(2, 4100)})
	long := strings.Repeat("a", 127)
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: long, Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offload.Run(context.Background(), p, nil); err != nil {
		t.Fatal(err)
	}
	dir := p.Dests[0]
	before := snapshot(t, dir)
	for _, dry := range []bool{true, false} {
		_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "{name}_{name}_{n}", DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "255") {
			t.Fatalf("dry %v: %v", dry, err)
		}
	}
	if snapshot(t, dir) != before {
		t.Fatal("the tree changed")
	}
	// A temp name over the limit: the frame's own name is long already.
	b := dngtest.Build(t, frame(3, 1000))
	longName := strings.Repeat("b", 240) + ".DNG"
	d2 := t.TempDir()
	os.WriteFile(filepath.Join(d2, longName), b, 0o644)
	if _, err := rename.Run(context.Background(), rename.Options{Dir: d2, Pattern: "x{n}"}); err == nil || !strings.Contains(err.Error(), "255") {
		t.Fatalf("long temp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d2, longName)); err != nil {
		t.Fatal(err)
	}
}

// IMPORTANT 2: a folder that can't be written is refused before anything moves (a
// locked frame: fix1_darwin_test.go).
func TestRenameUnwritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000), "A2.DNG": frame(2, 1100), "keep/A3.DNG": frame(3, 1200)})
	before := snapshot(t, dir)
	keep := filepath.Join(dir, "keep")
	os.Chmod(keep, 0o555)
	defer os.Chmod(keep, 0o755)
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"})
	if err == nil || !strings.Contains(err.Error(), "keep") || !strings.Contains(err.Error(), "written") {
		t.Fatalf("unwritable: %v", err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("the tree changed")
	}
}

// IMPORTANT 3: rename and redate refuse while a judge or ranking batch is pending.
func TestRenameRefusesPendingBatch(t *testing.T) {
	for _, suffix := range []string{".batch.json", ".rank-batch.json"} {
		dir, _, _, _ := shoot(t)
		state := filepath.Join(dir, "cull-report.json"+suffix)
		os.WriteFile(state, []byte("{}"), 0o644)
		before := snapshot(t, dir)
		// Final review M4: the advice is the command that finishes it (no cancel command exists).
		want := "finish it first with `cull judge --batch " + journal.ShellQuote(dir) + "` (already paid for)"
		if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern}); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "cancel") {
			t.Fatalf("%s rename: %v", suffix, err)
		}
		if _, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: time.Now()}); err == nil || !strings.Contains(err.Error(), "batch") {
			t.Fatalf("%s redate: %v", suffix, err)
		}
		if snapshot(t, dir) != before {
			t.Fatal("the tree changed")
		}
	}
}

// IMPORTANT 4: the folder lock. A rename is refused while another command holds the
// folder (judge's shared lock), naming it; judge (the command) is refused while a
// rename holds it.
func TestRenameFolderLock(t *testing.T) {
	dir, _, _, _ := shoot(t)
	release, _, err := journal.Lock(dir, false, "judge")
	if err != nil {
		t.Fatal(err)
	}
	_, err = rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: pattern})
	if err == nil || !strings.Contains(err.Error(), "cull judge") {
		t.Fatalf("rename while judge holds the folder: %v", err)
	}
	if _, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: time.Now()}); err == nil || !strings.Contains(err.Error(), "cull judge") {
		t.Fatalf("redate while judge holds the folder: %v", err)
	}
	release()
	release, _, err = journal.Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cullCmd("judge", "--backend", "openai", "--model", "m", dir); err == nil || !strings.Contains(err.Error(), "cull rename") {
		t.Fatalf("judge while rename holds the folder: %v\n%s", err, out)
	}
	release()
	// The lock file is no frame, temp or record.
	if out, err := cullCmd("status", dir); err != nil || strings.Contains(out, ".cull.lock") || !strings.Contains(out, ": 5 DNGs") {
		t.Fatalf("status: %v\n%s", err, out)
	}
}

// IMPORTANT 4: offload into a shoot folder with an unfinished rename is refused.
func TestAdvOffloadDuringUnfinished(t *testing.T) {
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000), "M2.DNG": frame(2, 4100)})
	dir := offloadCard(t, c, dest)
	judge(t, dir, &counting{}, nil)
	restore := rename.SetCrash(func(step string, i int) bool { return step == "phase1" && i == 1 })
	rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x_{n:3}"})
	restore()
	_, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err == nil || !strings.Contains(err.Error(), "cull rename") {
		t.Fatalf("offload during an unfinished rename: %v", err)
	}
	// Held by a rename: an offload into it is refused too.
	runRename(t, rename.Options{Dir: dir, Pattern: "x_{n:3}"})
	release, _, err := journal.Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err == nil {
		_, err = offload.Run(context.Background(), p, nil)
	}
	if err == nil || !strings.Contains(err.Error(), "cull rename") {
		t.Fatalf("offload while a rename holds the folder: %v", err)
	}
}

// IMPORTANT 5: a frame with other companions (a camera JPG, a ".DNG.xmp", Capture
// One's settings) or a new name whose stem something holds is refused before anything
// moves, naming the files.
func TestRenameCompanions(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"jpg", "A1.JPG"},
		{"dng-xmp", "A1.DNG.xmp"},
		{"capture one", "CaptureOne/Settings153/A1.DNG.cos"},
		{"new stem held", "x1.pp3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
			loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000), "A2.DNG": frame(2, 1100)})
			p := filepath.Join(dir, tc.file)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("x"), 0o644)
			os.WriteFile(filepath.Join(dir, "A1.xmp"), []byte("<x:xmpmeta/>"), 0o644) // moves with it: fine
			os.WriteFile(filepath.Join(dir, "._A1.DNG"), []byte("ad"), 0o644)         // AppleDouble: fine
			before := snapshot(t, dir)
			_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"})
			if err == nil || !strings.Contains(err.Error(), filepath.Base(tc.file)) {
				t.Fatalf("%v", err)
			}
			if snapshot(t, dir) != before {
				t.Fatal("the tree changed")
			}
		})
	}
}

// Minor: two frames with one camera name (two bodies) in a shoot, one deleted: the
// deleted frame's manifest name counts as taken, and the manifest follows the
// survivor by (camera name, size).
func TestAdvManifestDupOrig(t *testing.T) {
	c1, c2, dest := t.TempDir(), t.TempDir(), t.TempDir()
	card(t, c1, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000)})
	card(t, c2, map[string]dngtest.Fixture{"M1.DNG": frame(2, 4500)})
	var dir string
	for _, c := range []string{c1, c2} {
		p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02", Rename: "x_{n:2}"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := offload.Run(context.Background(), p, nil); err != nil {
			t.Fatal(err)
		}
		dir = p.Dests[0]
	}
	os.Remove(filepath.Join(dir, "x_01.DNG")) // the user deleted the first card's frame
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x_{n:2}"}); err == nil || !strings.Contains(err.Error(), "x_01.DNG") {
		t.Fatalf("a deleted frame's manifest name taken: %v", err)
	}
	runRename(t, rename.Options{Dir: dir, Pattern: "y_{n:2}"})
	st, err := os.Stat(filepath.Join(dir, "y_01.DNG"))
	if err != nil {
		t.Fatal(err)
	}
	es, _ := offload.CurrentManifest(dir)
	found := false
	for _, e := range es {
		if e.Size == st.Size() {
			found = e.Name == "y_01.DNG"
		} else if e.Name != "x_01.DNG" {
			t.Errorf("the deleted frame's entry now names %s", e.Name)
		}
	}
	if !found {
		t.Fatalf("the survivor's entry wasn't superseded: %+v", es)
	}
}

// Minor: hidden rename temps no journal records make a rename refuse (one may be a
// frame's only copy); an unreadable journal is never "moved aside".
func TestRenameOrphanTempsAndUnreadableJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000)})
	tmp := filepath.Join(dir, ".cull-rename-0a0b0c0d.A2.DNG")
	os.WriteFile(tmp, []byte("maybe a frame"), 0o644)
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"}); err == nil || !strings.Contains(err.Error(), tmp) {
		t.Fatalf("orphan temp: %v", err)
	}
	os.Remove(tmp)
	os.WriteFile(filepath.Join(dir, journal.RenameName), []byte("{torn"), 0o644)
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"})
	if err == nil || strings.Contains(err.Error(), "aside") || !strings.Contains(err.Error(), "cull status") {
		t.Fatalf("unreadable journal: %v", err)
	}
	if _, finish, ok := journal.Incomplete(dir); !ok || strings.Contains(finish, "--undo") {
		t.Fatalf("Incomplete on an unreadable journal: %q", finish)
	}
}

// Minor: another report in the folder is named in a warning (it doesn't follow).
func TestRenameWarnsOtherReports(t *testing.T) {
	dir, _, _, _ := shoot(t)
	os.WriteFile(filepath.Join(dir, "cull-report-old.json"), []byte("{}"), 0o644)
	var lines []string
	runRename(t, rename.Options{Dir: dir, Pattern: pattern, UI: notes(&lines)})
	if !strings.Contains(strings.Join(lines, "\n"), "cull-report-old.json") {
		t.Fatalf("no warning:\n%s", strings.Join(lines, "\n"))
	}
}

// Minor: a crash during the review-cache step leaves temps in assets/: the re-run sweeps
// them.
func TestRenameSweepsCacheTemps(t *testing.T) {
	dir, _, _, _ := shoot(t)
	stray := filepath.Join(dir, "cull-review", "assets", ".cull-rename-0a0b0c0d.M1.thumb.12345678.jpg")
	os.WriteFile(stray, []byte("x"), 0o644)
	runRename(t, rename.Options{Dir: dir, Pattern: pattern})
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("cache temp left")
	}
}

// From the review: a crash inside one move (a link made but the old name not yet
// removed; a frame moved but its sidecar not), resumed or undone.
func TestAdvCrashMidMove(t *testing.T) {
	for _, ph := range []string{"phase1", "phase2"} {
		for _, undo := range []bool{false, true} {
			name := ph
			if undo {
				name += "/undo"
			}
			t.Run(name, func(t *testing.T) {
				dir, data, where, b := swapShoot(t)
				restore := rename.SetCrash(func(step string, i int) bool { return step == ph && i == 0 })
				_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
				restore()
				if !errors.Is(err, rename.ErrCrash) {
					t.Fatal(err)
				}
				j, _ := journal.LoadRename(dir)
				abs := func(p string) string { return filepath.Join(dir, p) }
				m1, m2 := j.Moves[1], j.Moves[2]
				if ph == "phase1" {
					if err := os.Link(abs(m1.Old), abs(m1.Tmp)); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(abs(m2.Old), abs(m2.Tmp)); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Link(abs(m1.Tmp), abs(m1.New)); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(abs(m2.Tmp), abs(m2.New)); err != nil {
						t.Fatal(err)
					}
				}
				if undo {
					runRename(t, rename.Options{Dir: dir, Undo: true})
					follows(t, dir, data, where, where, swapLabelsAt(where), b)
					return
				}
				runRename(t, rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
				follows(t, dir, data, where, swapped, swapLabels(), b)
			})
		}
	}
}

// From the review: case-only renames (m1.dng → m1.DNG) on a case-insensitive volume.
func TestAdvCaseOnly(t *testing.T) {
	c, dest := t.TempDir(), t.TempDir()
	data := card(t, c, map[string]dngtest.Fixture{"m1.dng": frame(1, 4000), "m2.dng": frame(2, 4100)})
	dir := offloadCard(t, c, dest)
	b := &counting{}
	judge(t, dir, b, nil)
	label(t, dir, "m1.dng", "keep", 4)
	buildSheet(t, dir)
	if res := runRename(t, rename.Options{Dir: dir, Pattern: "{orig}"}); res.Renamed != 2 {
		t.Fatalf("%+v", res)
	}
	from := map[string]string{"m1.dng": "m1.dng", "m2.dng": "m2.dng"}
	to := map[string]string{"m1.dng": "m1.DNG", "m2.dng": "m2.DNG"}
	want := map[string]*labels.Entry{"m1.dng": {Label: "keep", Stars: 4}}
	follows(t, dir, data, from, to, want, b)
	runRename(t, rename.Options{Dir: dir, Undo: true})
	follows(t, dir, data, to, from, want, b)
}
