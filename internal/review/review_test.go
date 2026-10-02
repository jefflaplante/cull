package review

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
)

// tinyDNG writes a minimal DNG whose only preview is a 1600×1067 JPEG.
func tinyDNG(t *testing.T, path string) {
	t.Helper()
	var j bytes.Buffer
	jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 1600, 1067)), nil)
	le := binary.LittleEndian
	var b bytes.Buffer
	b.WriteString("II")
	binary.Write(&b, le, uint16(42))
	binary.Write(&b, le, uint32(8))
	dataOff := uint32(8 + 2 + 4*12 + 4)
	binary.Write(&b, le, uint16(4))
	for _, e := range [][3]uint32{{0x00FE, 4, 1}, {0x0103, 3, 7}, {0x0111, 4, dataOff}, {0x0117, 4, uint32(j.Len())}} {
		binary.Write(&b, le, uint16(e[0]))
		binary.Write(&b, le, uint16(e[1]))
		binary.Write(&b, le, uint32(1))
		binary.Write(&b, le, e[2])
	}
	binary.Write(&b, le, uint32(0))
	b.Write(j.Bytes())
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildWritesAnOfflinePageWithImages(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	f1 := filepath.Join(dir, "L1.DNG")
	f2 := filepath.Join(dir, `L2<b>&".DNG`) // hostile name: must stay inert in the page
	tinyDNG(t, f1)
	tinyDNG(t, f2)
	pv := &report.PreviewInfo{Width: 1600, Height: 1067, Orientation: 1, Source: "tiff-ifd"}
	rep := &report.Report{Dir: dir, Results: []report.Result{
		{File: f1, Preview: pv, Decision: eval.Cull, Reasons: []string{"sharpness: missed_focus"},
			Evaluation:  &eval.Evaluation{Sharpness: eval.Sharpness{Score: 2, Status: "missed_focus", FocusTarget: "ear"}},
			FocusTarget: &report.FocusTarget{Source: "face", FaceQ: 120, Box: &eval.NormBox{Left: 0.4, Top: 0.3, Right: 0.6, Bottom: 0.5}}},
		{File: f2, Preview: pv, FocusTarget: &report.FocusTarget{Source: "none", Reason: "no face"}}, // scan-style
		{File: filepath.Join(dir, "broken.DNG"), Error: "preview: boom"},
	}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: out, Concurrency: 2}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	index := sheet.Index
	for _, f := range []string{"L1.thumb.jpg", "L1.subject.jpg", `L2<b>&".thumb.jpg`} {
		if _, err := os.Stat(filepath.Join(out, "assets", f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "assets", `L2<b>&".subject.jpg`)); err == nil {
		t.Error("a frame without a focus box got a subject crop")
	}
	b, _ := os.ReadFile(index)
	page := string(b)
	for _, want := range []string{`id="data"`, "Export labels", "L1.DNG", "missed_focus", "boom", "cull-labels.jsonl", `"assets/"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"http://", "https://", "L2<b>", "labels.csv"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

func TestPageMarksServeMode(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	rep := &report.Report{Dir: dir, Results: []report.Result{{File: filepath.Join(dir, "L1.DNG")}}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	static, _ := os.ReadFile(sheet.Index)
	served, err := sheet.Page(true)
	if err != nil || !strings.Contains(string(served), `"serve":true`) || strings.Contains(string(static), `"serve"`) {
		t.Fatalf("serve flag: %v", err)
	}
}

func TestPageSaysWhereTheFilesAre(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	rep := &report.Report{Dir: dir, Results: []report.Result{{File: filepath.Join(dir, "L1.DNG")}}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	folder, _ := json.Marshal(dir)
	if page, _ := sheet.Page(false); !strings.Contains(string(page), `"folder":`+string(folder)) {
		t.Fatalf("page lacks the shoot folder %s", folder)
	}
}

// TestPageIncludesSetRankAndSummary checks the page carries a ranked frame's rank
// and its set's summary, so the page's JS can render the badge and detail rows.
func TestPageIncludesSetRankAndSummary(t *testing.T) {
	dir := t.TempDir()
	rep := &report.Report{Dir: dir, KeepBest: 1, Results: []report.Result{
		{File: filepath.Join(dir, "L1.DNG"), Group: &report.Group{ID: 1, Size: 3, Rank: 1, Of: 3, By: "model", Best: true, Strength: "sharp eyes", Weakness: "busy background"}},
		{File: filepath.Join(dir, "L2.DNG"), Group: &report.Group{ID: 1, Size: 3, Rank: 2, Of: 3, By: "model", Best: false, Strength: "good light", Weakness: "eyes closed"}},
		{File: filepath.Join(dir, "L3.DNG"), Group: &report.Group{ID: 1, Size: 3, Rank: 3, Of: 3, By: "model", Best: false}},
	}, Sets: []report.Set{
		{ID: 1, Members: []string{"L1.DNG", "L2.DNG", "L3.DNG"}, Of: 3, By: "model", Summary: "L1 is the sharpest and best lit."},
	}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sheet.Index)
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{`"rank":2`, `"set_summary":"L1 is the sharpest and best lit."`, `"keep_best":1`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestPageOmitsStaleSummaryForScoresSets checks that a set which fell back to
// by-scores ranking never shows a summary carried over from an earlier model
// ranking: strength/weakness are already blanked for such sets (groups.go), and
// the summary must be too.
func TestPageOmitsStaleSummaryForScoresSets(t *testing.T) {
	dir := t.TempDir()
	rep := &report.Report{Dir: dir, KeepBest: 1, Results: []report.Result{
		{File: filepath.Join(dir, "L1.DNG"), Group: &report.Group{ID: 1, Size: 2, Rank: 1, Of: 2, By: "scores", Best: true}},
		{File: filepath.Join(dir, "L2.DNG"), Group: &report.Group{ID: 1, Size: 2, Rank: 2, Of: 2, By: "scores", Best: false}},
	}, Sets: []report.Set{
		{ID: 1, Members: []string{"L1.DNG", "L2.DNG"}, Of: 2, By: "scores", Summary: "stale: L1 was the model's pick last time"},
	}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sheet.Index)
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if strings.Contains(page, "stale: L1 was the model's pick last time") {
		t.Error("page shows a summary carried over into a set that fell back to by-scores ranking")
	}
	if strings.Contains(page, `"set_summary"`) {
		t.Error("page has a set_summary field for a by-scores set")
	}
}

// A v3 report's legacy groups (size only, no current rank; report.Load drops
// them) must not surface as a stale "set 0 · #0/0" badge in the built page before
// the first decide/rank.
func TestBuildOfV3ReportHasNoSetBadge(t *testing.T) {
	dir := t.TempDir()
	f1, f2 := filepath.Join(dir, "L1.DNG"), filepath.Join(dir, "L2.DNG")
	tinyDNG(t, f1)
	tinyDNG(t, f2)
	v3 := fmt.Sprintf(`{"schema_version": 3, "backend": "anthropic", "model": "m", "dir": %q, "results": [
  {"file": %q, "size": 10, "group": {"id": 1, "size": 2, "best": "L2.DNG"}},
  {"file": %q, "size": 11, "group": {"id": 1, "size": 2}}]}`, dir, f1, f2)
	p := filepath.Join(dir, "r.json")
	if err := os.WriteFile(p, []byte(v3), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := report.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if r.Group != nil {
			t.Fatalf("premise: report.Load must drop legacy groups, got %+v", r.Group)
		}
	}
	sheet, err := Build(rep, p, Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sheet.Index)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"group":{`) {
		t.Errorf("v3 report's legacy group must not reach the page data:\n%s", b)
	}
}

func TestBuildMovesLooseImagesIntoAssets(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	f := filepath.Join(dir, "L1.DNG")
	tinyDNG(t, f)
	// A sheet built before assets/ existed: images loose beside index.html.
	os.WriteFile(filepath.Join(out, "L1.thumb.jpg"), []byte("old thumb"), 0o644)
	os.WriteFile(filepath.Join(out, "other.jpg"), []byte("not ours"), 0o644)
	os.WriteFile(filepath.Join(out, "report.json"), []byte("{}"), 0o644)
	rep := &report.Report{Dir: dir, Results: []report.Result{
		{File: f, Preview: &report.PreviewInfo{Width: 1600, Height: 1067, Orientation: 1, Source: "tiff-ifd"}},
	}}
	if _, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: out, Concurrency: 1}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "assets", "L1.thumb.jpg")); string(b) != "old thumb" {
		t.Fatalf("loose thumbnail not moved (re-rendered or missing): %q", b)
	}
	if _, err := os.Stat(filepath.Join(out, "L1.thumb.jpg")); err == nil {
		t.Error("loose thumbnail still beside index.html")
	}
	for _, keep := range []string{"other.jpg", "report.json"} {
		if _, err := os.Stat(filepath.Join(out, keep)); err != nil {
			t.Errorf("%s was touched: %v", keep, err)
		}
	}
}

// The page's own behaviour is checked in a browser; these pin that the handlers ship.
func TestPageHasNavigationKeysAndRevert(t *testing.T) {
	for _, want := range []string{`k === "n"`, `k === "]"`, `k === "["`, `prev:`, "N next unlabelled", "(reverted)"} {
		if !strings.Contains(pageTemplate, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestPageHasSetCompare(t *testing.T) {
	for _, want := range []string{`k === "s"`, `id="compare"`, "S compare set"} {
		if !strings.Contains(pageTemplate, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// The page's pure functions run under node when it is installed.
func TestPagePureFunctions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := filepath.Join(t.TempDir(), "page.html")
	os.WriteFile(page, []byte(pageTemplate), 0o644)
	out, err := exec.Command(node, "testdata/page_pure_test.js", page).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// Label keys in the loupe act on the frame it shows and never advance the view.
func TestLoupeLabelsTheFrameItShows(t *testing.T) {
	if !strings.Contains(pageTemplate, "labelCard(loupeIdx") {
		t.Fatal("loupe label keys don't target the loupe's frame")
	}
}

// Sets render as boxed blocks with keeper controls; keys - and = change the count.
func TestPageHasSetBlocksAndKeeperControls(t *testing.T) {
	for _, want := range []string{"setblock", `k === "-"`, `k === "="`, "keeper-toggle", "- = keepers"} {
		if !strings.Contains(pageTemplate, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// Groups are named group_n in the page, and the model's "best" pill is gone.
func TestPageNamesGroupsWithoutBestPill(t *testing.T) {
	if !strings.Contains(pageTemplate, "group_") || strings.Contains(pageTemplate, "bestPill(") {
		t.Fatal("page still shows the best pill or lacks group_n names")
	}
}

// Exposure: a linear-light SVG filter for the preview, keys , . < > \ and a slider.
func TestPageHasExposureControls(t *testing.T) {
	for _, want := range []string{`color-interpolation-filters="linearRGB"`, `k === ","`, `k === "."`, `k === "\\"`, `type: "range"`, ", . exposure"} {
		if !strings.Contains(pageTemplate, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
