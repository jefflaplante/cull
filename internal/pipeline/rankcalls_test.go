package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/report"
)

// grid builds a LookSize×LookSize×3 look fingerprint: a uniform background with
// one differently-shaded "subject" cell, so two grids with the subject at
// different positions have a real, controllable, nonzero LookDistance (mirrors
// group_test.go's scene() at grid scale; RankCalls never decodes an image when
// Look is already set, so a real DNG isn't needed to drive it).
func grid(bg, subj uint8, sx, sy int) []uint8 {
	g := make([]uint8, group.LookSize*group.LookSize*3)
	for y := 0; y < group.LookSize; y++ {
		for x := 0; x < group.LookSize; x++ {
			v := bg
			if x == sx && y == sy {
				v = subj
			}
			for c := 0; c < 3; c++ {
				g[(y*group.LookSize+x)*3+c] = v
			}
		}
	}
	return g
}

var sharpEval = &eval.Evaluation{
	Sharpness:   eval.Sharpness{Score: 8, Status: "sharp"},
	Exposure:    eval.Exposure{Status: "good"},
	Composition: eval.Composition{Status: "good"},
	People:      eval.People{Present: true, Eyes: "open", Expression: "good"},
}

func TestRankCallsCountsExactCalls(t *testing.T) {
	c, rep := seqShoot(t, 5) // one set of 5 identical frames
	sets, calls, filled, err := RankCalls(context.Background(), rep, c, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 0 {
		t.Fatalf("filled = %d, want 0: seqShoot already computed every look", filled)
	}
	if sets != 1 || calls != 1 {
		t.Fatalf("sets=%d calls=%d, want 1, 1 (one set of 5 fits in a single call)", sets, calls)
	}
	if len(rep.Sets) != 1 || rep.Sets[0].By == "model" {
		t.Fatalf("RankCalls must not mutate rep's own sets: %+v", rep.Sets)
	}
}

func TestRankCallsForceCountsAlreadyRankedSets(t *testing.T) {
	c, rep := seqShoot(t, 5)
	b := &rankBackend{order: reverse}
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 2}, false); err != nil {
		t.Fatal(err)
	}
	decideAll(rep, c.Policy, c.Seq)
	if rep.Sets[0].By != "model" {
		t.Fatalf("setup: the set should be fully ranked: %+v", rep.Sets[0])
	}

	sets, calls, _, err := RankCalls(context.Background(), rep, c, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sets != 0 || calls != 0 {
		t.Fatalf("without --force, a fully-ranked set must not be counted: sets=%d calls=%d", sets, calls)
	}

	sets, calls, _, err = RankCalls(context.Background(), rep, c, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sets != 1 || calls != 1 {
		t.Fatalf("--force must count the already-ranked set: sets=%d calls=%d", sets, calls)
	}
}

func TestRankCallsFillsLooksOnAV3ReportAndCountsForReal(t *testing.T) {
	c, rep := seqShoot(t, 5)
	rep.SchemaVersion = 3
	for i := range rep.Results {
		rep.Results[i].Look = "" // simulate a v3 report: no look computed yet
	}
	rep.Sets = nil // a v3 report also has no sets

	sets, calls, filled, err := RankCalls(context.Background(), rep, c, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 5 {
		t.Fatalf("filled = %d, want 5: every frame's look needed computing", filled)
	}
	if sets != 1 || calls != 1 {
		t.Fatalf("sets=%d calls=%d, want 1, 1: the real regrouping still finds one set of 5", sets, calls)
	}
	for i, r := range rep.Results {
		if r.Look == "" {
			t.Fatalf("result %d: look not filled in rep; the caller needs to persist it", i)
		}
	}
	if rep.Sets != nil {
		t.Fatal("RankCalls must not write rep's own Sets: only fillLooks may mutate rep")
	}
}

func TestRankCallsCountChangesWithSeqLook(t *testing.T) {
	gA := grid(100, 220, 2, 2)
	gB := grid(100, 220, 6, 5) // subject moved far: a real, sizeable look distance
	d := group.LookDistance(gA, gB)
	if d <= 0.02 {
		t.Fatalf("premise: need a clearly nonzero look distance, got %.4f", d)
	}
	newRep := func() *report.Report {
		return &report.Report{Results: []report.Result{
			{File: "/x/L1.DNG", Preview: &report.PreviewInfo{Orientation: 1}, Look: report.EncodeLook(gA), Evaluation: sharpEval},
			{File: "/x/L2.DNG", Preview: &report.PreviewInfo{Orientation: 1}, Look: report.EncodeLook(gB), Evaluation: sharpEval},
		}}
	}
	cfg := Config{Seq: group.Options{Gap: time.Minute, MaxLook: d + 0.02}, Policy: eval.Policy{KeepBest: 3, Outranked: eval.ActionReview}}

	sets, calls, _, err := RankCalls(context.Background(), newRep(), cfg, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sets != 1 || calls != 1 {
		t.Fatalf("a lenient --seq-look should link the pair into one set: sets=%d calls=%d", sets, calls)
	}

	cfg.Seq.MaxLook = d - 0.02
	if cfg.Seq.MaxLook < 0 {
		t.Fatalf("premise: distance %.4f too small for this test", d)
	}
	sets, calls, _, err = RankCalls(context.Background(), newRep(), cfg, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sets != 0 || calls != 0 {
		t.Fatalf("a strict --seq-look should not link the pair: sets=%d calls=%d", sets, calls)
	}
}

func TestRankCallsMakesNoModelCallsAndMovesNothing(t *testing.T) {
	c, rep := seqShoot(t, 3)
	if _, _, _, err := RankCalls(context.Background(), rep, c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "cull")); err == nil {
		t.Fatal("RankCalls must not move anything")
	}
	for i := 1; i <= 3; i++ {
		if _, err := os.Stat(filepath.Join(c.Dir, fmt.Sprintf("L%07d.DNG", i))); err != nil {
			t.Fatalf("frame missing: %v", err)
		}
	}
}

func loadReport(t *testing.T, path string) *report.Report {
	t.Helper()
	rep, err := report.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// scanShoot scans n identical textured frames: one set, nothing judged.
func scanShoot(t *testing.T, n int) (Config, []string) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for i := 1; i <= n; i++ {
		f := filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i))
		texturedDNG(t, f)
		files = append(files, f)
	}
	c := moveCfg(dir)
	c.MoveCulled, c.DryRun = false, true
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	return c, files
}

func TestEstimateRankingFromScanSets(t *testing.T) {
	c, files := scanShoot(t, 9)
	rep := loadReport(t, c.ReportPath)
	e := EstimateRanking(rep, files, c)
	if !e.Grouped || e.Sets != 1 || e.Calls != 3 { // 9 frames: 2 chunks + a final
		t.Fatalf("got %+v", e)
	}
	if e := EstimateRanking(nil, files, c); e.Grouped || e.Calls != 2 { // no report: ⌈9/8⌉
		t.Fatalf("no report: %+v", e)
	}
	c3, f3 := scanShoot(t, 3)
	c3.RankTwice = true
	if e := EstimateRanking(loadReport(t, c3.ReportPath), f3, c3); e.Calls != 2 { // one call + its reverse
		t.Fatalf("twice: %+v", e)
	}
}
