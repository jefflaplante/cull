package review

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
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
