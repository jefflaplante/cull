package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// minimalDNG: single IFD0 marked reduced-resolution + JPEG, strip = a black JPEG.
func minimalDNG(t *testing.T, path string) {
	dngWith(t, path, image.NewRGBA(image.Rect(0, 0, 1600, 1067)))
}

// texturedDNG embeds random 8px blocks so "focus landed" cells exist.
func texturedDNG(t *testing.T, path string) {
	img := image.NewGray(image.Rect(0, 0, 1600, 1067))
	rng := rand.New(rand.NewSource(3))
	for by := 0; by < 1067; by += 8 {
		for bx := 0; bx < 1600; bx += 8 {
			v := uint8(60 + rng.Intn(140))
			for y := by; y < by+8 && y < 1067; y++ {
				for x := bx; x < bx+8 && x < 1600; x++ {
					img.Pix[y*1600+x] = v
				}
			}
		}
	}
	dngWith(t, path, img)
}

func dngWith(t *testing.T, path string, img image.Image) {
	var j bytes.Buffer
	jpeg.Encode(&j, img, &jpeg.Options{Quality: 95})
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

// fakeBackend answers every call with an evaluation whose sharpness status is fixed.
type fakeBackend struct {
	status string
	calls  int32
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	atomic.AddInt32(&f.calls, 1)
	js := fmt.Sprintf(`{"sharpness":{"score":8,"status":%q,"focus_target":""},
 "exposure":{"score":7,"status":"fixable","ev_adjust":0.5,"clipping":"none","reason":""},
 "composition":{"score":6,"status":"croppable","issues":[],"crop":{"apply":true,"left":0.1,"top":0,"right":1,"bottom":1},"straighten_degrees":0},
 "notes":""}`, f.status)
	return &llm.Response{JSON: json.RawMessage(js), Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func cfg(dir string) Config {
	return Config{Dir: dir, ReportPath: filepath.Join(dir, "r.json"), Concurrency: 2, WriteXMP: true, XMPDevelop: true,
		MinPreviewEdge: 1500, Prep: imageprep.Options{MaxEdge: 800}, LandedTiles: 1, Policy: eval.Policy{MinCropArea: 0.6},
		CheckpointN: 1, Log: io.Discard}
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.dng"))

	rep, usage, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"})
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
	rep2, usage2, err := Run(context.Background(), c, &fakeBackend{status: "missed_focus"})
	if err != nil || usage2.InputTokens != 0 || len(rep2.Results) != 2 || rep2.Results[0].Decision != eval.Keep {
		t.Fatalf("resume re-evaluated: err=%v usage=%+v", err, usage2)
	}
}

// scriptedBackend returns a valid evaluation, with err injected on call number errOn.
type scriptedBackend struct {
	calls int32
	errOn int32
	err   error
}

func (s *scriptedBackend) Name() string { return "scripted" }
func (s *scriptedBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	n := atomic.AddInt32(&s.calls, 1)
	resp, _ := (&fakeBackend{status: "sharp"}).Call(ctx, req)
	if n == s.errOn {
		if errors.Is(s.err, llm.ErrQuotaStop) {
			return resp, s.err // quota stop still delivers this call's result
		}
		return nil, s.err
	}
	return resp, nil
}

func fourFiles(t *testing.T) string {
	dir := t.TempDir()
	for i := 1; i <= 4; i++ {
		minimalDNG(t, filepath.Join(dir, fmt.Sprintf("L100000%d.DNG", i)))
	}
	return dir
}

func TestQuotaStopKeepsResultsAndResumeFinishes(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Concurrency, c.WriteXMP = 1, false
	_, _, err := Run(context.Background(), c, &scriptedBackend{errOn: 2, err: fmt.Errorf("%w: 95%% used", llm.ErrQuotaStop)})
	if !errors.Is(err, llm.ErrQuotaStop) {
		t.Fatalf("want ErrQuotaStop, got %v", err)
	}
	saved, lerr := report.Load(c.ReportPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	ok := 0
	for _, r := range saved.Results {
		if r.Error == "" && r.Evaluation != nil {
			ok++
		}
	}
	if ok != 2 || len(saved.Results) != 2 {
		t.Fatalf("after quota stop: %d results, %d ok; want 2 and 2", len(saved.Results), ok)
	}

	c.Resume = true
	b := &scriptedBackend{}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil || len(rep.Results) != 4 || b.calls != 2 {
		t.Fatalf("resume: err=%v results=%d calls=%d", err, len(rep.Results), b.calls)
	}
}

func TestAbortRunStopsDispatch(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Concurrency, c.WriteXMP = 1, false
	b := &scriptedBackend{errOn: 1, err: fmt.Errorf("%w: wrong auth", llm.ErrAbortRun)}
	rep, _, err := Run(context.Background(), c, b)
	if !errors.Is(err, llm.ErrAbortRun) || b.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, b.calls)
	}
	if len(rep.Results) != 1 || rep.Results[0].Error == "" {
		t.Fatalf("want the aborted frame recorded as an error, got %+v", rep.Results)
	}
}

func TestResumeRefusesDifferentBackendOrSchema(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model = "openai", "local-model"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	c.Resume, c.Backend = true, "claude-code"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "drop --resume") {
		t.Fatalf("backend mismatch: want refusal, got %v", err)
	}

	os.WriteFile(c.ReportPath, []byte(`{"schema_version":1,"model":"local-model","backend":"openai","results":[]}`), 0o644)
	c.Backend = "openai"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "drop --resume") {
		t.Fatalf("schema mismatch: want refusal, got %v", err)
	}
}

// routedBackend answers by schema name and records the call sequence.
type routedBackend struct {
	mu      sync.Mutex
	locate  string // JSON returned for focus_target
	locErr  error
	calls   []string
	evalReq *llm.Request
}

func (r *routedBackend) Name() string { return "routed" }
func (r *routedBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req.SchemaName)
	r.mu.Unlock()
	if req.SchemaName == "focus_target" {
		if r.locErr != nil && !errors.Is(r.locErr, llm.ErrQuotaStop) {
			return nil, r.locErr
		}
		return &llm.Response{JSON: json.RawMessage(r.locate), Usage: llm.Usage{InputTokens: 900, OutputTokens: 40}}, r.locErr
	}
	r.mu.Lock()
	r.evalReq = &req
	r.mu.Unlock()
	return (&fakeBackend{status: "sharp"}).Call(ctx, req)
}

func requestText(req *llm.Request) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Parts {
		b.WriteString(p.Text + "\n")
	}
	return b.String()
}

func TestFocusTargetFlow(t *testing.T) {
	face := func(*imageprep.Frame) []focus.Face {
		return []focus.Face{{Rect: image.Rect(400, 300, 600, 500), Q: 120}}
	}
	weakFace := func(*imageprep.Frame) []focus.Face { // below --face-min-q
		return []focus.Face{{Rect: image.Rect(10, 10, 60, 60), Q: 30}}
	}
	const eye = `{"confident":true,"kind":"eye","subject":"left eye","box":{"left":0.4,"top":0.3,"right":0.45,"bottom":0.35}}`
	cases := []struct {
		name       string
		detect     func(*imageprep.Frame) []focus.Face
		locate     string
		locErr     error
		locateOff  bool
		dryRun     bool
		wantCalls  string
		wantSource string
		wantReason string
		wantText   string
		wantErr    error
	}{
		{name: "face found", detect: face, wantCalls: "evaluation", wantSource: "face", wantText: "face detector"},
		{name: "model locates", detect: weakFace, locate: eye, wantCalls: "focus_target evaluation", wantSource: "model", wantText: "model: left eye"},
		{name: "no clear subject", detect: weakFace, locate: `{"confident":false,"kind":"none","subject":"","box":{"left":0,"top":0,"right":1,"bottom":1}}`,
			wantCalls: "focus_target evaluation", wantSource: "none", wantReason: "no clear subject", wantText: "No subject crop"},
		{name: "invalid box", detect: weakFace, locate: `{"confident":true,"kind":"face","subject":"x","box":{"left":0.6,"top":0.3,"right":0.4,"bottom":0.5}}`,
			wantCalls: "focus_target evaluation", wantSource: "none", wantReason: "invalid box", wantText: "No subject crop"},
		{name: "locate off", detect: weakFace, locateOff: true, wantCalls: "evaluation", wantSource: "none", wantReason: "locate off"},
		{name: "locate fails", detect: weakFace, locErr: errors.New("boom"), wantCalls: "focus_target evaluation", wantSource: "none", wantReason: "locate failed"},
		{name: "quota stop during locate", detect: weakFace, locate: eye, locErr: fmt.Errorf("%w: 95%%", llm.ErrQuotaStop),
			wantCalls: "focus_target evaluation", wantSource: "model", wantErr: llm.ErrQuotaStop},
		{name: "scan never calls a backend", detect: face, dryRun: true, wantCalls: "", wantSource: "face"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
			conf := cfg(dir)
			conf.WriteXMP, conf.FaceMinQ, conf.Locate, conf.DryRun, conf.detect = false, 80, !c.locateOff, c.dryRun, c.detect
			rb := &routedBackend{locate: c.locate, locErr: c.locErr}
			var backend llm.Backend = rb
			if c.dryRun {
				backend = nil
			}
			rep, _, err := Run(context.Background(), conf, backend)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if got := strings.Join(rb.calls, " "); got != c.wantCalls {
				t.Fatalf("calls %q, want %q", got, c.wantCalls)
			}
			r := rep.Results[0]
			ft := r.FocusTarget
			if r.Error != "" || ft == nil || ft.Source != c.wantSource || !strings.Contains(ft.Reason, c.wantReason) {
				t.Fatalf("result error=%q focus_target=%+v", r.Error, ft)
			}
			if !c.dryRun && r.Evaluation == nil {
				t.Fatal("frame was not evaluated")
			}
			if !strings.Contains(requestText(rb.evalReq), c.wantText) {
				t.Fatalf("evaluation request lacks %q:\n%s", c.wantText, requestText(rb.evalReq))
			}
			switch c.wantSource {
			case "face":
				if ft.FaceQ != 120 || ft.Faces != 1 || ft.Box == nil || ft.Box.Left != 0.25 {
					t.Fatalf("face target %+v box %+v", ft, ft.Box)
				}
			case "model":
				if ft.Label != "left eye" || ft.Box == nil || *ft.Box != (eval.NormBox{Left: 0.4, Top: 0.3, Right: 0.45, Bottom: 0.35}) {
					t.Fatalf("model target %+v box %+v", ft, ft.Box)
				}
			}
		})
	}
}

func TestReportV2OnDisk(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model, c.WriteXMP = "openai", "local-model", false
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	if _, _, err := Run(context.Background(), c, &routedBackend{locate: `{"confident":false,"kind":"none","subject":"","box":{"left":0,"top":0,"right":1,"bottom":1}}`}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.ReportPath)
	for _, want := range []string{`"schema_version": 2`, `"backend": "openai"`, `"focus_target"`, `"source": "none"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report lacks %s", want)
		}
	}
}

func TestSaveInputsRecordsExactlyWhatIsSent(t *testing.T) {
	for _, dry := range []bool{false, true} {
		dir, out := t.TempDir(), t.TempDir()
		texturedDNG(t, filepath.Join(dir, "L1000001.DNG"))
		c := cfg(dir)
		c.WriteXMP, c.FaceMinQ, c.DryRun, c.SaveInputs = false, 80, dry, out
		c.detect = func(*imageprep.Frame) []focus.Face {
			return []focus.Face{{Rect: image.Rect(400, 300, 600, 500), Q: 120}}
		}
		var b llm.Backend = &routedBackend{}
		if dry {
			b = nil
		}
		if _, _, err := Run(context.Background(), c, b); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"L1000001.full.jpg", "L1000001.subject.jpg", "L1000001.landed-1.jpg", "L1000001.inputs.json"} {
			if _, err := os.Stat(filepath.Join(out, f)); err != nil {
				t.Fatalf("dry=%v: missing %s", dry, f)
			}
		}
		raw, _ := os.ReadFile(filepath.Join(out, "L1000001.inputs.json"))
		var rec struct {
			SchemaName  string                         `json:"schema_name"`
			Parts       []struct{ Text, Image string } `json:"parts"`
			FocusTarget *report.FocusTarget            `json:"focus_target"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		var seq []string
		for _, p := range rec.Parts {
			if p.Image != "" {
				seq = append(seq, p.Image)
			}
		}
		want := "L1000001.full.jpg L1000001.subject.jpg L1000001.landed-1.jpg"
		if got := strings.Join(seq, " "); got != want || rec.SchemaName != "evaluation" || rec.FocusTarget == nil || rec.FocusTarget.Source != "face" {
			t.Fatalf("dry=%v: images %q schema %q focus %+v", dry, got, rec.SchemaName, rec.FocusTarget)
		}
		if !strings.Contains(rec.Parts[len(rec.Parts)-1].Text, "mean luma") {
			t.Fatalf("dry=%v: last part is not the stats text: %+v", dry, rec.Parts[len(rec.Parts)-1])
		}
	}
}
