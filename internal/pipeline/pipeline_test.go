package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
)

// minimalDNG: single IFD0 marked reduced-resolution + JPEG, strip = the JPEG.
func minimalDNG(t *testing.T, path string) {
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

type fakeEval struct{ status string }

func (f fakeEval) Evaluate(ctx context.Context, in eval.Input) (*eval.Evaluation, eval.Usage, error) {
	return &eval.Evaluation{
		Sharpness:   eval.Sharpness{Score: 8, Status: f.status},
		Exposure:    eval.Exposure{Score: 7, Status: "fixable", EVAdjust: 0.5},
		Composition: eval.Composition{Score: 6, Status: "croppable", Crop: eval.Crop{Apply: true, Left: 0.1, Top: 0, Right: 1, Bottom: 1}},
	}, eval.Usage{InputTokens: 100, OutputTokens: 10}, nil
}

func cfg(dir string) Config {
	return Config{Dir: dir, ReportPath: filepath.Join(dir, "r.json"), Concurrency: 2, WriteXMP: true, XMPDevelop: true,
		MinPreviewEdge: 1500, Prep: imageprep.Options{MaxEdge: 800, Tiles: 2}, Policy: eval.Policy{MinCropArea: 0.6},
		CheckpointN: 1, Log: io.Discard}
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.dng"))

	rep, usage, err := Run(context.Background(), cfg(dir), fakeEval{"sharp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 || usage.InputTokens != 200 {
		t.Fatalf("results=%d usage=%+v", len(rep.Results), usage)
	}
	for _, r := range rep.Results {
		if r.Error != "" || r.Decision != eval.Keep || r.XMP == "" {
			t.Fatalf("bad result %+v", r)
		}
		x, _ := os.ReadFile(r.XMP)
		if !strings.Contains(string(x), `crs:Exposure2012="+0.50"`) || !strings.Contains(string(x), "gophotocull:keep") {
			t.Fatalf("sidecar missing fields:\n%s", x)
		}
	}

	// Resume: nothing re-evaluated; a failing evaluator proves it isn't called.
	c := cfg(dir)
	c.Resume = true
	rep2, usage2, err := Run(context.Background(), c, fakeEval{"missed_focus"})
	if err != nil || usage2.InputTokens != 0 || len(rep2.Results) != 2 || rep2.Results[0].Decision != eval.Keep {
		t.Fatalf("resume re-evaluated: err=%v usage=%+v", err, usage2)
	}
}
