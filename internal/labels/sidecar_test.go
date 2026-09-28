package labels

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

func TestSidecarMapping(t *testing.T) {
	for _, c := range []struct {
		name  string
		model eval.Decision
		l     Entry
		want  xmp.Sidecar
	}{
		{"model keep", eval.Keep, Entry{}, xmp.Sidecar{Label: "Green", Keywords: []string{"gophotocull:keep"}}},
		{"model review", eval.Review, Entry{}, xmp.Sidecar{Label: "Yellow", Keywords: []string{"gophotocull:review"}}},
		{"your keep over model cull", eval.Cull, Entry{Label: "keep", Stars: 4},
			xmp.Sidecar{Rating: 4, Label: "Green", Keywords: []string{"gophotocull:keep", "gophotocull:labeled"}}},
		{"stars only", eval.Review, Entry{Stars: 2}, xmp.Sidecar{Rating: 2, Label: "Yellow", Keywords: []string{"gophotocull:review"}}},
		{"scan frame you culled", "", Entry{Label: "cull"}, xmp.Sidecar{Label: "Red", Keywords: []string{"gophotocull:cull", "gophotocull:labeled"}}},
		{"nothing", "", Entry{}, xmp.Sidecar{}},
	} {
		got := Sidecar(report.Result{Decision: c.model}, c.l, 1, false)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSidecarDevelopNeverForCulls(t *testing.T) {
	e := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.5},
		Composition: eval.Composition{Crop: eval.Crop{Apply: true, Left: 0.1, Top: 0.1, Right: 0.9, Bottom: 0.9}}}
	r := report.Result{Decision: eval.Keep, Evaluation: e}
	if sc := Sidecar(r, Entry{}, 1, true); sc.ExposureEV == nil || sc.Crop == nil {
		t.Fatalf("keep: develop missing: %+v", sc)
	}
	if sc := Sidecar(r, Entry{Label: "cull"}, 1, true); sc.ExposureEV != nil || sc.Crop != nil {
		t.Fatalf("your cull still got develop settings: %+v", sc)
	}
}

func TestWriteSidecarOwnership(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep}
	p := filepath.Join(dir, "L1.xmp")
	if err := WriteSidecar(&r, Entry{Stars: 3}, false, false); err != nil || r.XMP != p {
		t.Fatalf("create: %v %q", err, r.XMP)
	}
	if err := WriteSidecar(&r, Entry{Stars: 5}, false, false); err != nil {
		t.Fatalf("ours must be rewritten: %v", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `xmp:Rating="5"`) {
		t.Fatalf("not rewritten:\n%s", b)
	}
	foreign := report.Result{File: filepath.Join(dir, "L2.DNG"), Decision: eval.Keep}
	os.WriteFile(filepath.Join(dir, "L2.xmp"), []byte("foreign"), 0o644)
	if err := WriteSidecar(&foreign, Entry{}, false, false); !errors.Is(err, xmp.ErrExists) || foreign.XMP != "" {
		t.Fatalf("foreign: %v %q", err, foreign.XMP)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L2.xmp")); string(b) != "foreign" {
		t.Fatal("foreign sidecar overwritten")
	}
	moved := report.Result{File: filepath.Join(dir, "L3.DNG"), MovedTo: filepath.Join(dir, "culled", "L3.DNG"), Decision: eval.Cull}
	os.MkdirAll(filepath.Join(dir, "culled"), 0o755)
	if err := WriteSidecar(&moved, Entry{}, false, false); err != nil || moved.XMP != filepath.Join(dir, "culled", "L3.xmp") {
		t.Fatalf("moved: %v %q", err, moved.XMP)
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
