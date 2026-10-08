package develop

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

func TestFromReport(t *testing.T) {
	results := []report.Result{
		{File: "/s/A.DNG", Decision: eval.Keep, Exif: &dng.Exif{Model: "LEICA M10-R"}},
		{File: "/s/B.DNG", Decision: eval.Keep, MovedTo: "/s/keep/B.DNG"},    // sorted: develop it where it is
		{File: "/s/C.DNG", Decision: eval.Review},                            // the model's review, your keep
		{File: "/s/D.DNG", Decision: eval.Keep},                              // the model's keep, your cull
		{File: "/s/E.DNG", Decision: eval.Cull},                              // nobody's keep
		{File: "/s/F.DNG", Decision: eval.Review},                            // unlabelled review: not delivered
		{File: "/s/G.DNG", Decision: eval.Keep, Error: "preview: truncated"}, // still a keep
	}
	labs := map[string]labels.Entry{
		"C.DNG": {File: "C.DNG", Label: "keep"},
		"D.DNG": {File: "D.DNG", Label: "cull"},
		"A.DNG": {File: "A.DNG", Stars: 4}, // stars only: the model's verdict stands
	}
	got := FromReport(results, labs)
	want := []Keep{
		{Name: "A.DNG", Path: "/s/A.DNG", Model: "LEICA M10-R"},
		{Name: "B.DNG", Path: "/s/keep/B.DNG"},
		{Name: "C.DNG", Path: "/s/C.DNG"},
		{Name: "G.DNG", Path: "/s/G.DNG"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestFromLabels(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"A.DNG", "keep/B.DNG", "raw/C.DNG", "raw/D.DNG", ".hidden/E.DNG"} {
		touch(t, filepath.Join(dir, p))
	}
	labs := map[string]labels.Entry{
		"A.DNG": {File: "A.DNG", Label: "keep"},
		"B.DNG": {File: "B.DNG", Label: "keep"},
		"C.DNG": {File: "C.DNG", Label: "keep"},
		"D.DNG": {File: "D.DNG", Label: "cull"},
		"E.DNG": {File: "E.DNG", Label: "keep"}, // hidden folders are never searched
		"F.DNG": {File: "F.DNG", Label: "keep"}, // gone
	}
	cases := []struct {
		recursive bool
		want      []Keep
		missing   []string
	}{
		{false, []Keep{{Name: "A.DNG", Path: filepath.Join(dir, "A.DNG")}, {Name: "B.DNG", Path: filepath.Join(dir, "keep/B.DNG")}},
			[]string{"C.DNG", "E.DNG", "F.DNG"}},
		{true, []Keep{{Name: "A.DNG", Path: filepath.Join(dir, "A.DNG")}, {Name: "B.DNG", Path: filepath.Join(dir, "keep/B.DNG")},
			{Name: "C.DNG", Path: filepath.Join(dir, "raw/C.DNG")}}, []string{"E.DNG", "F.DNG"}},
	}
	for _, c := range cases {
		got, missing, err := FromLabels(dir, c.recursive, labs)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(missing, c.missing) {
			t.Errorf("recursive %v: got %+v missing %v\nwant %+v missing %v", c.recursive, got, missing, c.want, c.missing)
		}
	}
}

// Labels name frames by base name, so two frames with one name can't be told apart.
func TestFromLabelsRefusesDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "a/X.DNG"))
	touch(t, filepath.Join(dir, "b/X.DNG"))
	_, _, err := FromLabels(dir, true, map[string]labels.Entry{"X.DNG": {File: "X.DNG", Label: "keep"}})
	if err == nil || !strings.Contains(err.Error(), "X.DNG") {
		t.Fatalf("err %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "A.DNG")
	if err := os.WriteFile(p, dngtest.Build(t, dngtest.Fixture{Make: "Leica Camera AG", Model: "LEICA M10-R"}), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(Keep{Name: "A.DNG", Path: p})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if f.Model != "LEICA M10-R" || f.Size != st.Size() || !f.ModTime.Equal(st.ModTime()) || f.Sidecar != "" || f.EV != nil {
		t.Errorf("no sidecar: %+v", f)
	}
	if err := xmp.Write(xmp.Path(p), xmp.Sidecar{Label: "Green", ExposureEV: ev(0.5), Crop: &xmp.Box{Left: 0.1, Top: 0.1, Right: 0.9, Bottom: 0.9}}, false); err != nil {
		t.Fatal(err)
	}
	f, err = Load(Keep{Name: "A.DNG", Path: p, Model: "FROM REPORT"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "FROM REPORT" || f.Sidecar != xmp.Path(p) || f.SidecarSum == "" || f.EV == nil || *f.EV != 0.5 || !f.Crop {
		t.Errorf("with sidecar: %+v", f)
	}
	if _, err := Load(Keep{Name: "B.DNG", Path: filepath.Join(dir, "B.DNG")}); err == nil {
		t.Error("missing DNG: no error")
	}
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
