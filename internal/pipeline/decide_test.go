package pipeline

import (
	"context"
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

func TestBurstDuplicatesGoToReviewAndDecideCanCullThem(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"L1000001", "L1000002", "L1000003"} { // identical frames, no EXIF
		texturedDNG(t, filepath.Join(dir, n+".DNG"))
	}
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, true
	c.GroupGap = 2 * time.Second
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	best := result(t, rep, "L1000001.DNG") // equal scores: capture (file-name) order decides
	if g := best.Group; g == nil || g.ID != 1 || g.Size != 3 || g.Rank != 1 || g.Of != 3 || g.By != "scores" || !g.Best ||
		best.Decision != eval.Keep || best.Look == "" {
		t.Fatalf("best: group=%+v decision=%s look=%q", best.Group, best.Decision, best.Look)
	}
	for rank, n := range []string{"L1000002.DNG", "L1000003.DNG"} {
		r := result(t, rep, n)
		if g := r.Group; g == nil || g.ID != 1 || g.Rank != rank+2 || g.Best {
			t.Fatalf("%s: group=%+v", n, r.Group)
		}
		if r.Decision != eval.Review || !strings.Contains(strings.Join(r.Reasons, ";"), "duplicate of L1000001.DNG (burst of 3)") {
			t.Fatalf("%s: decision=%s reasons=%v", n, r.Decision, r.Reasons)
		}
		if b, _ := os.ReadFile(r.XMP); !strings.Contains(string(b), `xmp:Label="Yellow"`) {
			t.Fatalf("%s: sidecar not updated for the duplicate decision:\n%s", n, b)
		}
	}

	sum, err := Decide(c.ReportPath, DecideOptions{
		Policy: eval.Policy{MinCropArea: 0.6, Duplicates: eval.ActionCull}, GroupGap: 2 * time.Second,
	}, io.Discard)
	if err != nil || sum.Changed["review→cull"] != 2 {
		t.Fatalf("decide: %+v %v", sum, err)
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
	if err := finishRun(rep, Config{ReportPath: rp, Dir: dir, MoveCulled: true, Labels: lab, Log: &log}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "share a file name") || !exists(filepath.Join(dir, "a", CulledDir, "L1.DNG")) {
		t.Fatalf("cull fallback: %s", log.String())
	}
}
