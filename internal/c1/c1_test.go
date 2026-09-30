package c1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
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

// block returns one frame's part of the script.
func block(s, name string) string {
	i := strings.Index(s, "\n\t-- "+name+":")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	if j := strings.Index(rest[1:], "\n\t-- "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

func TestScriptColorsAndKeywordsWithoutLabels(t *testing.T) {
	s := Script(testReport(), Options{Rating: true, Label: true, Keyword: true})
	for name, tag := range map[string]string{"L1.DNG": "4", "L2.DNG": "3", `L"3\.DNG`: "1"} {
		if b := block(s, name); !strings.Contains(b, "set color tag of v to "+tag) {
			t.Errorf("%s: want color tag %s:\n%s", name, tag, b)
		}
	}
	for _, want := range []string{`matchImages(doc, "/s/L\"3\\.DNG", "L\"3\\.DNG", "L\"3\\")`, `"cull:cull"`, `"cull:keep"`} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s", want)
		}
	}
	for _, bad := range []string{"set rating", "cull:labeled", "L4.DNG"} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %s (ratings come only from your stars)", bad)
		}
	}
}

func TestScriptBestKeyword(t *testing.T) {
	rep := testReport()
	rep.Results[0].Group = &report.Group{ID: 1, Size: 2, Rank: 1, Of: 2, Best: true}
	rep.Results[1].Group = &report.Group{ID: 1, Size: 2, Rank: 2, Of: 2, Best: false}
	s := Script(rep, Options{Keyword: true})
	b1, b2 := block(s, "L1.DNG"), block(s, "L2.DNG")
	if !strings.Contains(b1, `"cull:best"`) {
		t.Errorf("L1 (best of its set): want cull:best\n%s", b1)
	}
	if strings.Contains(b2, `"cull:best"`) {
		t.Errorf("L2 (not best): got cull:best\n%s", b2)
	}
}

func TestScriptUsesYourLabelsAndStars(t *testing.T) {
	lab := map[string]labels.Entry{
		"L1.DNG": {File: "L1.DNG", Label: "cull"}, // model keep, you cull
		"L2.DNG": {File: "L2.DNG", Stars: 4},      // model review, your stars
	}
	s := Script(testReport(), Options{Rating: true, Label: true, Keyword: true, Labels: lab})
	b1, b2, b3 := block(s, "L1.DNG"), block(s, "L2.DNG"), block(s, `L"3\.DNG`)
	if !strings.Contains(b1, "set color tag of v to 1") || !strings.Contains(b1, `"cull:labeled"`) || strings.Contains(b1, "set rating") {
		t.Errorf("L1:\n%s", b1)
	}
	if !strings.Contains(b2, "set rating of v to 4") || !strings.Contains(b2, "set color tag of v to 3") || strings.Contains(b2, "labeled") {
		t.Errorf("L2:\n%s", b2)
	}
	if !strings.Contains(b3, "set color tag of v to 1") || strings.Contains(b3, "set rating") {
		t.Errorf("L3:\n%s", b3)
	}
}

func TestScriptDevelopOnlyWhenAskedAndApplicable(t *testing.T) {
	s := Script(testReport(), Options{Exposure: true, Crop: true})
	if !strings.Contains(s, `set exposure of adjustments of v to 0.7`) || strings.Count(s, "set exposure of adjustments") != 1 {
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

func TestScriptNameCannotEscapeComment(t *testing.T) {
	rep := &report.Report{Results: []report.Result{{File: "/s/x\ndo shell script \"say pwned\"\n\r¬.DNG", Decision: eval.Keep}}}
	s := Script(rep, Options{Label: true})
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "do shell script") {
			t.Fatalf("file name escaped into code: %q", line)
		}
	}
	if strings.ContainsAny(s, "\r") {
		t.Fatal("raw CR in script")
	}
}

func TestScriptMatchesByPathAndListsAmbiguousNames(t *testing.T) {
	rep := &report.Report{Results: []report.Result{
		{File: "/s/L1.DNG", Decision: eval.Keep},
		{File: "/s/L2.DNG", Decision: eval.Cull, MovedTo: "/s/culled/L2.DNG"},
	}}
	s := Script(rep, Options{Label: true})
	if !strings.Contains(s, `matchImages(doc, "/s/L1.DNG", "L1.DNG", "L1")`) {
		t.Errorf("L1 not matched by its path:\n%s", block(s, "L1.DNG"))
	}
	if !strings.Contains(s, `matchImages(doc, "/s/culled/L2.DNG", "L2.DNG", "L2")`) {
		t.Errorf("moved L2 not matched where it lives now:\n%s", block(s, "L2.DNG"))
	}
	for _, want := range []string{"whose path is posixPath", "(count of found) is 1", "ambiguous"} {
		if !strings.Contains(s, want) {
			t.Errorf("helpers lack %q", want)
		}
	}
}

func TestQuoteEscapesControlCharacters(t *testing.T) {
	if got := quote("a\"b\\c\nd\re\tf"); got != `"a\"b\\c\nd\re\tf"` {
		t.Fatalf("got %s", got)
	}
}

func TestScriptAppliesEditsOnlyOverDefaults(t *testing.T) {
	s := Script(testReport(), Options{Exposure: true, Crop: true})
	b := block(s, "L1.DNG")
	if !strings.Contains(b, "if (exposure of adjustments of v) is 0 then set exposure of adjustments of v to 0.7") {
		t.Errorf("exposure not guarded:\n%s", b)
	}
	if !strings.Contains(b, "if my isFullFrame(crop of v, w, h) then set crop of v to") {
		t.Errorf("crop not guarded:\n%s", b)
	}
	if !strings.Contains(s, "on isFullFrame(") {
		t.Error("helper missing")
	}
}
