package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/rawclip"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// culled runs cull on the three-frame shoot (L1 cull, L2 keep at score 8, L3 review).
func culledShoot(t *testing.T, mutate func(*Config)) (dir string, c Config) {
	t.Helper()
	dir, b := shoot(t)
	c = moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, false
	if mutate != nil {
		mutate(&c)
	}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

func TestDecideReappliesPolicyWithoutModel(t *testing.T) {
	_, c := culledShoot(t, nil)
	sum, err := Decide(c.ReportPath, DecideOptions{Policy: eval.Policy{MinCropArea: 0.6, ReviewBelowSharpness: 9}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Frames != 3 || sum.Changed["keep→review"] != 1 || len(sum.Changed) != 1 {
		t.Fatalf("summary %+v", sum)
	}
	rep, _ := report.Load(c.ReportPath)
	if r := result(t, rep, "L1000002.DNG"); r.Decision != eval.Review || !strings.Contains(strings.Join(r.Reasons, ";"), "below 9.0") {
		t.Fatalf("L2 decision=%s reasons=%v", r.Decision, r.Reasons)
	}
}

func TestDecideRewritesOnlyOurSidecars(t *testing.T) {
	dir, c := culledShoot(t, nil)
	foreign := filepath.Join(dir, "L1000003.xmp")
	os.WriteFile(foreign, []byte("foreign"), 0o644)

	opts := DecideOptions{Policy: eval.Policy{MinCropArea: 0.6}, WriteXMP: true}
	if _, err := Decide(c.ReportPath, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	l2 := filepath.Join(dir, "L1000002.xmp")
	if b, _ := os.ReadFile(l2); !strings.Contains(string(b), `xmp:Label="Green"`) || strings.Contains(string(b), "xmp:Rating") {
		t.Fatalf("keep sidecar: green, and no stars from the model:\n%s", b)
	}
	opts.Policy.ReviewBelowSharpness = 9 // L2 becomes review
	if _, err := Decide(c.ReportPath, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l2); !strings.Contains(string(b), `xmp:Label="Yellow"`) {
		t.Fatalf("our sidecar not rewritten for the new decision:\n%s", b)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "foreign" {
		t.Fatal("a sidecar we did not write was overwritten")
	}
	rep, _ := report.Load(c.ReportPath)
	if r := result(t, rep, "L1000003.DNG"); !strings.Contains(strings.Join(r.Fixups, ";"), "not ours") {
		t.Fatalf("no note for the foreign sidecar: %v", r.Fixups)
	}
}

func TestDecideSyncsMovedFrames(t *testing.T) {
	dir, c := culledShoot(t, func(c *Config) { c.MoveCulled = true })
	if !exists(filepath.Join(dir, "culled", "L1000001.DNG")) {
		t.Fatal("setup: L1 not moved")
	}
	// Simulate a re-evaluation: L1 is fine after all, L2 blinked.
	rep, _ := report.Load(c.ReportPath)
	for i := range rep.Results {
		switch filepath.Base(rep.Results[i].File) {
		case "L1000001.DNG":
			rep.Results[i].Evaluation.Sharpness.Status = "sharp"
		case "L1000002.DNG":
			rep.Results[i].Evaluation.People = eval.People{Present: true, Eyes: "closed"}
		}
	}
	rep.Save(c.ReportPath)

	sum, err := Decide(c.ReportPath, DecideOptions{Policy: eval.Policy{MinCropArea: 0.6, EyesClosed: eval.ActionCull}, MoveCulled: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Moved != 1 || sum.Restored != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if !exists(filepath.Join(dir, "L1000001.DNG")) || !exists(filepath.Join(dir, "culled", "L1000002.DNG")) {
		t.Fatal("L1 not restored or L2 not moved")
	}
}

func TestDecideNeedsEvaluations(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.DryRun, c.MoveCulled = true, false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(c.ReportPath, DecideOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "no evaluations") {
		t.Fatalf("scan report: %v", err)
	}
}

func TestSequenceOutrankedGoToReviewAndDecideCanCullThem(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"L1000001", "L1000002", "L1000003"} { // identical frames, no EXIF
		texturedDNG(t, filepath.Join(dir, n+".DNG"))
	}
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, true
	c.Seq = group.Options{Gap: 2 * time.Second, MaxLook: group.DefaultLook}
	c.Policy.KeepBest = 1 // Outranked "" = review
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	best := result(t, rep, "L1000001.DNG") // equal scores: capture (file-name) order decides
	if g := best.Group; g == nil || g.ID != 1 || g.Size != 3 || g.Rank != 1 || g.Of != 3 || g.By != "scores" || !g.Best ||
		best.Decision != eval.Keep || best.Look == "" {
		t.Fatalf("best: group=%+v decision=%s look=%q", best.Group, best.Decision, best.Look)
	}
	if len(rep.Sets) != 1 || len(rep.Sets[0].Members) != 3 || rep.KeepBest != 1 {
		t.Fatalf("sets %+v keepBest %d", rep.Sets, rep.KeepBest)
	}
	for rank, n := range []string{"L1000002.DNG", "L1000003.DNG"} {
		r := result(t, rep, n)
		if g := r.Group; g == nil || g.ID != 1 || g.Rank != rank+2 || g.Best {
			t.Fatalf("%s: group=%+v", n, r.Group)
		}
		want := fmt.Sprintf("rank %d of 3 in set 1 by scores, not compared (keeping the best 1)", rank+2)
		if r.Decision != eval.Review || !strings.Contains(strings.Join(r.Reasons, ";"), want) {
			t.Fatalf("%s: decision=%s reasons=%v", n, r.Decision, r.Reasons)
		}
		if b, _ := os.ReadFile(r.XMP); !strings.Contains(string(b), `xmp:Label="Yellow"`) {
			t.Fatalf("%s: sidecar not updated for the outranked decision:\n%s", n, b)
		}
	}

	sum, err := Decide(c.ReportPath, DecideOptions{
		Policy: eval.Policy{MinCropArea: 0.6, KeepBest: 1, Outranked: eval.ActionCull}, Seq: c.Seq,
	}, io.Discard)
	if err != nil || sum.Changed["review→cull"] != 2 {
		t.Fatalf("decide: %+v %v", sum, err)
	}
}

// setReport: n frames in one visual sequence 10 s apart, all evaluated keep with
// sharpness scores from sharp[], looks identical.
func setReport(t *testing.T, sharp ...float64) *report.Report {
	t.Helper()
	look := report.EncodeLook(make([]uint8, 192))
	rep := &report.Report{}
	for i, s := range sharp {
		e := &eval.Evaluation{Sharpness: eval.Sharpness{Status: "sharp", Score: s}}
		rep.Results = append(rep.Results, report.Result{File: fmt.Sprintf("/s/L%03d.DNG", i+1), Look: look,
			Preview: &report.PreviewInfo{Width: 100, Height: 100, Orientation: 1}, Evaluation: e, Decision: eval.Keep})
	}
	return rep
}

var seq = group.Options{Gap: time.Minute, MaxLook: 0.1}

func TestKeepBestByScoresWithoutRanking(t *testing.T) {
	rep := setReport(t, 7, 9, 8, 6, 9.5)
	decideAll(rep, eval.Policy{KeepBest: 3, Outranked: eval.ActionReview}, seq)
	want := map[string]eval.Decision{"L001": eval.Review, "L002": eval.Keep, "L003": eval.Keep, "L004": eval.Review, "L005": eval.Keep}
	for _, r := range rep.Results {
		if d := want[strings.TrimSuffix(filepath.Base(r.File), ".DNG")]; r.Decision != d || r.Group == nil || r.Group.By != "scores" {
			t.Errorf("%s: %s group=%+v", r.File, r.Decision, r.Group)
		}
	}
	if len(rep.Sets) != 1 || rep.KeepBest != 3 || len(needsRanking(rep)) != 1 {
		t.Fatalf("sets %+v keepBest %d needs %v", rep.Sets, rep.KeepBest, needsRanking(rep))
	}
}

func TestStoredModelOrderWinsAndIsReused(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9)
	p := eval.Policy{KeepBest: 2, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L001.DNG", "/s/L004.DNG", "/s/L002.DNG"}
	rep.Sets[0].By = "model"
	decideAll(rep, p, seq)
	got := map[string]int{}
	for _, r := range rep.Results {
		got[filepath.Base(r.File)] = r.Group.Rank
	}
	if got["L003.DNG"] != 1 || got["L002.DNG"] != 4 || len(needsRanking(rep)) != 0 {
		t.Fatalf("ranks %v needs %v", got, needsRanking(rep))
	}
}

func TestDecideReusesOrderWhenMembersDrop(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9)
	p := eval.Policy{KeepBest: 2, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L001.DNG", "/s/L004.DNG", "/s/L002.DNG"}
	rep.Sets[0].By = "model"
	rep.Results[0].Evaluation.Sharpness.Status = "missed_focus" // L001 becomes a technical cull
	decideAll(rep, p, seq)
	if len(needsRanking(rep)) != 0 {
		t.Fatal("a dropped member must not throw away the paid order")
	}
	for _, r := range rep.Results {
		if filepath.Base(r.File) == "L004.DNG" && (r.Group.Rank != 2 || r.Decision != eval.Keep) {
			t.Fatalf("L004 moves up to rank 2 and stays keep: %+v %s", r.Group, r.Decision)
		}
	}
	rep.Results = append(rep.Results, report.Result{File: "/s/L005.DNG", Look: rep.Results[1].Look,
		Preview: rep.Results[1].Preview, Evaluation: &eval.Evaluation{Sharpness: eval.Sharpness{Status: "sharp", Score: 9}}})
	decideAll(rep, p, seq)
	if len(needsRanking(rep)) != 1 {
		t.Fatal("a new member makes the set unranked")
	}
}

func TestOutrankedReasonAndNeverPromotes(t *testing.T) {
	rep := setReport(t, 9, 8, 7, 6)
	rep.Results[3].Evaluation.Sharpness.Status = "soft" // policy says review on its own
	p := eval.Policy{KeepBest: 1, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	for _, r := range rep.Results[1:3] {
		if r.Decision != eval.Review || !strings.Contains(strings.Join(r.Reasons, ";"), "in set 1 by scores, not compared (keeping the best 1)") {
			t.Fatalf("%s: %s %v", r.File, r.Decision, r.Reasons)
		}
	}
	if r := rep.Results[0]; r.Decision != eval.Keep || !r.Group.Best {
		t.Fatalf("best: %s %+v", r.Decision, r.Group)
	}
	decideAll(rep, eval.Policy{KeepBest: 0, Outranked: eval.ActionReview}, seq)
	for _, r := range rep.Results[:3] {
		if r.Decision != eval.Keep {
			t.Fatalf("KeepBest 0 ranks only: %s %s", r.File, r.Decision)
		}
	}
}

func TestLoneSurvivorIsBest(t *testing.T) {
	rep := setReport(t, 9, 3, 2)
	rep.Results[1].Evaluation.Sharpness.Status = "missed_focus"
	rep.Results[2].Evaluation.Sharpness.Status = "motion_blur"
	decideAll(rep, eval.Policy{KeepBest: 3, Outranked: eval.ActionReview}, seq)
	if g := rep.Results[0].Group; g == nil || g.Rank != 1 || !g.Best || g.Of != 1 || len(needsRanking(rep)) != 0 {
		t.Fatalf("lone survivor: %+v", g)
	}
	if g := rep.Results[1].Group; g == nil || g.Rank != 0 || g.Best {
		t.Fatalf("culled member stays in the set, unranked: %+v", g)
	}
}

func TestOutrankedNeverTouchesReview(t *testing.T) {
	rep := setReport(t, 9, 8, 7)
	rep.Results[2].Evaluation.Sharpness.Status = "soft"
	decideAll(rep, eval.Policy{KeepBest: 1, Outranked: eval.ActionCull}, seq)
	if r := rep.Results[1]; r.Decision != eval.Cull {
		t.Fatalf("outranked keep: %s %v", r.Decision, r.Reasons)
	}
	if r := rep.Results[2]; r.Decision != eval.Review || r.Group.Rank != 3 || strings.Contains(strings.Join(r.Reasons, ";"), "rank 3") {
		t.Fatalf("a review stays review, untouched by ranking: %s %v", r.Decision, r.Reasons)
	}
}

// A member the model ranked that a policy change culls, then a later decide
// restores, is still in the paid order. Together with
// TestUncoveredSetKeepsPaidOrderForLater: while the grouping stays the same, no
// decide discards a paid order, and undoing a policy change brings its ranks back.
func TestReturningMemberKeepsItsModelRank(t *testing.T) {
	rep := setReport(t, 9, 9, 9)
	rep.Results[1].Evaluation.People = eval.People{Present: true, Eyes: "closed"}
	decideAll(rep, eval.Policy{KeepBest: 1}, seq) // eyes closed: review by default, still rankable
	rep.Sets[0].Order = []string{"/s/L002.DNG", "/s/L001.DNG", "/s/L003.DNG"}
	rep.Sets[0].Notes = []report.RankNote{{File: "/s/L002.DNG", Strength: "moment", Weakness: "blink"}}
	rep.Sets[0].By = "model"
	decideAll(rep, eval.Policy{KeepBest: 1, EyesClosed: eval.ActionCull}, seq)
	if g := rep.Results[1].Group; g.Rank != 0 || rep.Results[0].Group.Rank != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("culled L002 drops out, L001 moves up: %+v %+v", g, rep.Sets[0])
	}
	decideAll(rep, eval.Policy{KeepBest: 1}, seq)
	if g := rep.Results[1].Group; g.Rank != 1 || g.By != "model" || g.Strength != "moment" || len(needsRanking(rep)) != 0 {
		t.Fatalf("L002 back at rank 1 from the stored order: %+v needs %v", g, needsRanking(rep))
	}
}

// A frame culled at rank time was never compared. Loosening the policy makes it
// rankable, so the set falls back to scores and needs ranking, but the paid order
// stays in the report; tightening again brings the model's ranks back.
func TestUncoveredSetKeepsPaidOrderForLater(t *testing.T) {
	rep := setReport(t, 9, 9, 9)
	rep.Results[0].Evaluation.People = eval.People{Present: true, Eyes: "closed"}
	strict, loose := eval.Policy{KeepBest: 1, EyesClosed: eval.ActionCull}, eval.Policy{KeepBest: 1}
	decideAll(rep, strict, seq) // L001 culled: ranked without it
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L002.DNG"}
	rep.Sets[0].Notes = []report.RankNote{{File: "/s/L003.DNG", Strength: "moment", Weakness: "tilt"}}
	rep.Sets[0].By, rep.Sets[0].Summary, rep.Sets[0].CostUSD = "model", "L3 wins", 0.03

	decideAll(rep, loose, seq) // L001 now review, rankable, never compared
	s := rep.Sets[0]
	if s.By != "scores" || len(needsRanking(rep)) != 1 || rep.Results[0].Group.Rank != 1 || rep.Results[2].Group.Strength != "" {
		t.Fatalf("uncovered: ranked by scores and flagged: %+v %+v", s, rep.Results[0].Group)
	}
	if len(s.Order) != 2 || s.Order[0] != "/s/L003.DNG" || len(s.Notes) != 1 || s.Summary != "L3 wins" || s.CostUSD != 0.03 {
		t.Fatalf("the paid order must stay in the report: %+v", s)
	}

	decideAll(rep, strict, seq)
	l2, l3 := rep.Results[1], rep.Results[2]
	if rep.Sets[0].By != "model" || len(needsRanking(rep)) != 0 || l3.Group.Rank != 1 || l3.Group.Strength != "moment" || l2.Group.Rank != 2 {
		t.Fatalf("covered again: the model's ranks are back: %+v %+v %+v", rep.Sets[0], l3.Group, l2.Group)
	}
	if why := strings.Join(l2.Reasons, ";"); l2.Decision != eval.Review || !strings.Contains(why, "rank 2 of 2 in set 1 (keeping the best 1)") {
		t.Fatalf("L002 outranked by the model: %s %s", l2.Decision, why)
	}
}

// When a regrouping splits a ranked set, each part keeps its members' order, and
// the set's cost is counted once.
func TestSplitSetKeepsOrderAndCountsCostOnce(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9)
	p := eval.Policy{KeepBest: 1}
	decideAll(rep, p, seq)
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L001.DNG", "/s/L004.DNG", "/s/L002.DNG"}
	rep.Sets[0].By, rep.Sets[0].CostUSD = "model", 0.05
	bright := make([]uint8, 192) // upper half bright: far from the black look of L001–L002
	for i := range bright[:96] {
		bright[i] = 255
	}
	rep.Results[2].Look, rep.Results[3].Look = report.EncodeLook(bright), report.EncodeLook(bright)
	decideAll(rep, p, seq)
	if len(rep.Sets) != 2 || rep.Sets[0].By != "model" || rep.Sets[1].By != "model" {
		t.Fatalf("sets %+v", rep.Sets)
	}
	if o := rep.Sets[1].Order; len(o) != 2 || o[0] != "/s/L003.DNG" || o[1] != "/s/L004.DNG" {
		t.Fatalf("second part's order: %v", o)
	}
	if c := rep.Sets[0].CostUSD + rep.Sets[1].CostUSD; c != 0.05 {
		t.Fatalf("set cost counted %v, want 0.05 once", c)
	}
}

func TestRawClipMeasuredAndUsed(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG")) // preview only: no raw to measure
	c := moveCfg(dir)
	c.MoveCulled, c.RawClip = false, true
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	r := result(t, rep, "L1000001.DNG")
	if r.Evaluation == nil || r.RawClip != nil || !strings.Contains(strings.Join(r.Fixups, ";"), "raw clip") {
		t.Fatalf("eval=%v rawclip=%v fixups=%v", r.Evaluation != nil, r.RawClip, r.Fixups)
	}

	// A stored raw measurement overrides preview clipping when re-deciding.
	rep.Results[0].Evaluation.Exposure.Status = "clipped"
	rep.Results[0].RawClip = &rawclip.Result{HighlightPct: 0.01, WhiteLevel: 16383}
	decideAll(rep, eval.Policy{}, group.Options{})
	if d := rep.Results[0].Decision; d != eval.Keep {
		t.Fatalf("raw headroom should keep, got %s %v", d, rep.Results[0].Reasons)
	}
}

func TestMovedForeignSidecarStaysForeign(t *testing.T) {
	dir, b := shoot(t)
	foreign := filepath.Join(dir, "L1000001.xmp") // L1 will be culled and moved
	os.WriteFile(foreign, []byte("foreign"), 0o644)
	c := moveCfg(dir) // MoveCulled on
	c.WriteXMP = true // our sidecar can't be written: it's foreign
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "culled", "L1000001.xmp")
	opts := DecideOptions{Policy: eval.Policy{MinCropArea: 0.6}, WriteXMP: true}
	if _, err := Decide(c.ReportPath, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(moved); string(got) != "foreign" {
		t.Fatalf("decide overwrote a foreign sidecar that travelled with its frame: %q", got)
	}
	if _, err := Restore(c.ReportPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(c.ReportPath, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(foreign); string(got) != "foreign" {
		t.Fatalf("decide overwrote a restored foreign sidecar: %q", got)
	}
}

func TestDecideLabelsDriveSidecarsAndMoves(t *testing.T) {
	dir, c := culledShoot(t, nil) // model: L1 cull, L2 keep, L3 review
	pol := eval.Policy{MinCropArea: 0.6}
	if _, err := Decide(c.ReportPath, DecideOptions{Policy: pol, MoveCulled: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, CulledDir, "L1000001.DNG")); err != nil {
		t.Fatal("the model's cull was not moved")
	}
	lab := map[string]labels.Entry{
		"L1000001.DNG": {File: "L1000001.DNG", Label: "keep"}, // you overrule the model's cull
		"L1000002.DNG": {File: "L1000002.DNG", Label: "cull"}, // and cull a model keep
		"L1000003.DNG": {File: "L1000003.DNG", Stars: 5},      // stars only: the model's review stands
	}
	if _, err := Decide(c.ReportPath, DecideOptions{Policy: pol, WriteXMP: true, MoveCulled: true, Labels: lab}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1000001.DNG")); err != nil {
		t.Error("frame you labeled keep was not restored")
	}
	if _, err := os.Stat(filepath.Join(dir, CulledDir, "L1000002.DNG")); err != nil {
		t.Error("frame you labeled cull was not moved")
	}
	rep, _ := report.Load(c.ReportPath)
	if r := result(t, rep, "L1000001.DNG"); r.Decision != eval.Cull {
		t.Errorf("report decision replaced by your label: %s", r.Decision)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if s := read(filepath.Join(dir, "L1000001.xmp")); !strings.Contains(s, `xmp:Label="Green"`) || !strings.Contains(s, "<rdf:li>cull:labeled</rdf:li>") {
		t.Errorf("L1 sidecar:\n%s", s)
	}
	if s := read(filepath.Join(dir, CulledDir, "L1000002.xmp")); !strings.Contains(s, `xmp:Label="Red"`) {
		t.Errorf("L2 sidecar did not follow the frame into culled/:\n%s", s)
	}
	if s := read(filepath.Join(dir, "L1000003.xmp")); !strings.Contains(s, `xmp:Rating="5"`) || !strings.Contains(s, `xmp:Label="Yellow"`) || strings.Contains(s, "labeled") {
		t.Errorf("L3 sidecar:\n%s", s)
	}
}

func TestLabelsWithDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	missed := &eval.Evaluation{Sharpness: eval.Sharpness{Status: "missed_focus", Score: 2}}
	rep := &report.Report{Dir: dir}
	for _, sub := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o755)
		f := filepath.Join(dir, sub, "L1.DNG")
		os.WriteFile(f, []byte("dng"), 0o644)
		rep.Results = append(rep.Results, report.Result{File: f, Evaluation: missed, Decision: eval.Cull})
	}
	rp := filepath.Join(dir, "cull-report.json")
	rep.Save(rp)
	lab := map[string]labels.Entry{"L1.DNG": {File: "L1.DNG", Label: "keep"}}
	// decide refuses: a label couldn't say which frame it means.
	if _, err := Decide(rp, DecideOptions{Labels: lab, MoveCulled: true}, io.Discard); err == nil || !strings.Contains(err.Error(), "share a file name") {
		t.Fatalf("decide: %v", err)
	}
	if exists(filepath.Join(dir, "a", CulledDir, "L1.DNG")) || exists(filepath.Join(dir, "b", CulledDir, "L1.DNG")) {
		t.Fatal("decide moved frames before refusing")
	}
	// cull (the end of a paid run) warns and falls back to the model's verdicts.
	var log strings.Builder
	if _, err := finishRun(context.Background(), rep, Config{ReportPath: rp, Dir: dir, MoveCulled: true, Labels: lab, Log: &log}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "share a file name") || !exists(filepath.Join(dir, "a", CulledDir, "L1.DNG")) {
		t.Fatalf("cull fallback: %s", log.String())
	}
}
