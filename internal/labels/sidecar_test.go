package labels

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

func TestSidecarMapping(t *testing.T) {
	for _, c := range []struct {
		name  string
		model eval.Decision
		l     Entry
		want  xmp.Sidecar
	}{
		{"model keep", eval.Keep, Entry{}, xmp.Sidecar{Label: "Green", Keywords: []string{"cull:keep"}}},
		{"model review", eval.Review, Entry{}, xmp.Sidecar{Label: "Yellow", Keywords: []string{"cull:review"}}},
		{"your keep over model cull", eval.Cull, Entry{Label: "keep", Stars: 4},
			xmp.Sidecar{Rating: 4, Label: "Green", Keywords: []string{"cull:keep", "cull:labeled"}}},
		{"stars only", eval.Review, Entry{Stars: 2}, xmp.Sidecar{Rating: 2, Label: "Yellow", Keywords: []string{"cull:review"}}},
		{"scan frame you culled", "", Entry{Label: "cull"}, xmp.Sidecar{Label: "Red", Keywords: []string{"cull:cull", "cull:labeled"}}},
		{"nothing", "", Entry{}, xmp.Sidecar{}},
	} {
		got := Sidecar(report.Result{Decision: c.model}, c.l, 1, false, nil)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSidecarDevelopNeverForCulls(t *testing.T) {
	e := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.5},
		Composition: eval.Composition{Crop: eval.Crop{Apply: true, Left: 0.1, Top: 0.1, Right: 0.9, Bottom: 0.9}}}
	r := report.Result{Decision: eval.Keep, Evaluation: e}
	if sc := Sidecar(r, Entry{}, 1, true, nil); sc.ExposureEV == nil || sc.Crop == nil {
		t.Fatalf("keep: develop missing: %+v", sc)
	}
	if sc := Sidecar(r, Entry{Label: "cull"}, 1, true, nil); sc.ExposureEV != nil || sc.Crop != nil {
		t.Fatalf("your cull still got develop settings: %+v", sc)
	}
}

func TestWriteSidecarOwnership(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep}
	p := filepath.Join(dir, "L1.xmp")
	if err := WriteSidecar(&r, Entry{Stars: 3}, false, false, nil); err != nil || r.XMP != p {
		t.Fatalf("create: %v %q", err, r.XMP)
	}
	if err := WriteSidecar(&r, Entry{Stars: 5}, false, false, nil); err != nil {
		t.Fatalf("ours must be rewritten: %v", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `xmp:Rating="5"`) {
		t.Fatalf("not rewritten:\n%s", b)
	}
	foreign := report.Result{File: filepath.Join(dir, "L2.DNG"), Decision: eval.Keep}
	os.WriteFile(filepath.Join(dir, "L2.xmp"), []byte("foreign"), 0o644)
	if err := WriteSidecar(&foreign, Entry{}, false, false, nil); !errors.Is(err, xmp.ErrExists) || foreign.XMP != "" {
		t.Fatalf("foreign: %v %q", err, foreign.XMP)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L2.xmp")); string(b) != "foreign" {
		t.Fatal("foreign sidecar overwritten")
	}
	moved := report.Result{File: filepath.Join(dir, "L3.DNG"), MovedTo: filepath.Join(dir, "culled", "L3.DNG"), Decision: eval.Cull}
	os.MkdirAll(filepath.Join(dir, "culled"), 0o755)
	if err := WriteSidecar(&moved, Entry{}, false, false, nil); err != nil || moved.XMP != filepath.Join(dir, "culled", "L3.xmp") {
		t.Fatalf("moved: %v %q", err, moved.XMP)
	}
}

func TestSidecarBestKeyword(t *testing.T) {
	r := report.Result{Decision: eval.Keep, Group: &report.Group{ID: 1, Size: 4, Rank: 2, Of: 4, Best: true}}
	if sc := Sidecar(r, Entry{}, 1, false, nil); !reflect.DeepEqual(sc.Keywords, []string{"cull:keep", "cull:best"}) {
		t.Fatalf("%v", sc.Keywords)
	}
	r.Group.Best = false
	if sc := Sidecar(r, Entry{}, 1, false, nil); len(sc.Keywords) != 1 {
		t.Fatalf("%v", sc.Keywords)
	}
}

func TestDuplicates(t *testing.T) {
	rs := []report.Result{{File: "/a/L1.DNG"}, {File: "/a/L2.DNG"}, {File: "/b/L1.DNG"}}
	if d := Duplicates(rs); len(d) != 1 || !strings.Contains(d[0], "/a/L1.DNG") || !strings.Contains(d[0], "/b/L1.DNG") {
		t.Fatalf("%v", d)
	}
	if d := Duplicates(rs[:2]); len(d) != 0 {
		t.Fatalf("%v", d)
	}
}

// A crash between the sidecar write and the report's checkpoint leaves r.XMP
// empty; the sidecar is still ours and must be rewritable.
func TestWriteSidecarRewritesOwnUnrecordedSidecar(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: &eval.Evaluation{}}
	if err := WriteSidecar(&r, Entry{}, false, false, nil); err != nil {
		t.Fatal(err)
	}
	r.XMP, r.Decision = "", eval.Cull
	if err := WriteSidecar(&r, Entry{}, false, false, nil); err != nil {
		t.Fatalf("own sidecar treated as foreign: %v", err)
	}
	if b, _ := os.ReadFile(r.XMP); !strings.Contains(string(b), "cull:cull") {
		t.Fatal("not rewritten")
	}
}

func TestWriteSidecarLeavesForeignSidecar(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: &eval.Evaluation{}}
	os.WriteFile(filepath.Join(dir, "L1.xmp"), []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Capture One"/>`), 0o644)
	if err := WriteSidecar(&r, Entry{}, false, false, nil); !errors.Is(err, xmp.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}

func TestWriteSidecarRecordsDevelop(t *testing.T) {
	dir := t.TempDir()
	ev := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.5}}
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: ev}
	if err := WriteSidecar(&r, Entry{}, true, false, nil); err != nil || !r.XMPDevelop {
		t.Fatalf("err=%v develop=%v", err, r.XMPDevelop)
	}
}

// Your EV goes into the sidecar's crs:Exposure2012 (Adobe tools read it; Capture One
// doesn't, so apply-c1 carries it there) even without --xmp-develop, and wins over
// the model's suggestion.
func TestSidecarCarriesYourEV(t *testing.T) {
	ev := -0.3
	r := report.Result{Decision: eval.Keep, Evaluation: &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.8}}}
	for _, develop := range []bool{false, true} {
		sc := Sidecar(r, Entry{EV: &ev}, 1, develop, nil)
		if sc.ExposureEV == nil || *sc.ExposureEV != -0.3 {
			t.Fatalf("develop=%v: exposure %v", develop, sc.ExposureEV)
		}
	}
}

func TestSidecarKeywordsAndTags(t *testing.T) {
	tags := &report.Tags{Project: "Smith wedding", Location: "Forest Park, Portland", Keywords: []string{"family"}}
	r := report.Result{File: "/s/L1.DNG", Decision: eval.Keep, Evaluation: &eval.Evaluation{Keywords: []string{"portrait", "forest"}}}
	sc := Sidecar(r, Entry{}, 1, false, tags)
	if got := strings.Join(sc.Keywords, ","); got != "cull:keep,portrait,forest,Smith wedding,Forest Park, Portland,family" {
		t.Fatalf("keywords %q", got)
	}
	if got := strings.Join(sc.Hierarchy, ";"); got != "content|portrait;content|forest;project|Smith wedding;location|Forest Park, Portland" {
		t.Fatalf("hierarchy %q", got)
	}
	junk := report.Result{File: "/s/L2.DNG", Decision: eval.Cull, Junk: &report.JunkInfo{Kind: "black"}}
	sc = Sidecar(junk, Entry{}, 1, false, tags)
	if got := strings.Join(sc.Keywords, ","); got != "cull:cull,Smith wedding,Forest Park, Portland,family" {
		t.Fatalf("junk keywords %q", got)
	}
}

// A tag repeating a content keyword is written once.
func TestSidecarKeywordsDeduplicated(t *testing.T) {
	tags := &report.Tags{Keywords: []string{"portrait", "family"}}
	r := report.Result{File: "/s/L1.DNG", Decision: eval.Keep, Evaluation: &eval.Evaluation{Keywords: []string{"portrait"}}}
	sc := Sidecar(r, Entry{}, 1, false, tags)
	if got := strings.Join(sc.Keywords, ","); got != "cull:keep,portrait,family" {
		t.Fatalf("keywords %q", got)
	}
}
