package c1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func testReport() *report.Report {
	fixable := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.7},
		Composition: eval.Composition{Status: "croppable", Crop: eval.Crop{Apply: true, Left: 0.1, Top: 0.2, Right: 0.9, Bottom: 1}}}
	good := &eval.Evaluation{Exposure: eval.Exposure{Status: "good"}}
	return &report.Report{Results: []report.Result{
		{File: "/s/L1.DNG", Decision: eval.Keep, Evaluation: fixable},
		{File: "/s/L2.DNG", Decision: eval.Review, Evaluation: good},
		{File: `/s/L"3\.DNG`, Decision: eval.Cull, Evaluation: good},
		{File: "/s/L4.DNG", Error: "preview: boom"},
	}}
}

func TestScriptSetsRatingLabelKeyword(t *testing.T) {
	s := Script(testReport(), Options{Rating: true, Label: true, Keyword: true})
	for _, want := range []string{
		`tell application "Capture One"`,
		`matchImages(doc, "L1.DNG", "L1")`, `set rating of v to 3`,
		`matchImages(doc, "L2.DNG", "L2")`, `set rating of v to 2`, `set color tag of v to 3`,
		`matchImages(doc, "L\"3\\.DNG", "L\"3\\")`, `set rating of v to 1`, `set color tag of v to 1`,
		`"gophotocull:cull"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s", want)
		}
	}
	for _, bad := range []string{"L4.DNG", "exposure of adjustments", "set crop of v"} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %s", bad)
		}
	}
}

func TestScriptDevelopOnlyWhenAskedAndApplicable(t *testing.T) {
	s := Script(testReport(), Options{Exposure: true, Crop: true})
	if !strings.Contains(s, `set exposure of adjustments of v to 0.7`) || strings.Count(s, "exposure of adjustments") != 1 {
		t.Errorf("exposure: only the fixable frame\n%s", s)
	}
	if !strings.Contains(s, "set crop of v to {") || !strings.Contains(s, "0.5 * w") || strings.Count(s, "set crop of v") != 1 {
		t.Errorf("crop: only the croppable frame, scaled to the image\n%s", s)
	}
	if strings.Contains(s, "set rating") {
		t.Error("rating written without --rating")
	}
}

func TestProbeIsReadOnly(t *testing.T) {
	p := Probe(3)
	if !strings.Contains(p, `tell application "Capture One"`) || !strings.Contains(p, "color tag of v") || !strings.Contains(p, "dimensions of img") {
		t.Fatalf("probe lacks fields:\n%s", p)
	}
	for _, write := range []string{"set rating", "set color tag", "set crop", "apply keyword", "make new", "set exposure"} {
		if strings.Contains(p, write) {
			t.Errorf("probe writes: %s", write)
		}
	}
}

func TestRunPipesToOsascript(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "osascript")
	os.WriteFile(fake, []byte("#!/bin/sh\ncat > \""+dir+"/got.applescript\"\necho done\n"), 0o755)
	out, err := Run(fake, "tell application \"Capture One\"\nend tell\n")
	if err != nil || !strings.Contains(out, "done") {
		t.Fatalf("run: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "got.applescript")); !strings.Contains(string(b), "Capture One") {
		t.Fatalf("osascript got %q", b)
	}
}
