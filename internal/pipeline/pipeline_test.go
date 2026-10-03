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
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
)

// minimalDNG: single IFD0 marked reduced-resolution + JPEG, strip = a black JPEG.
// minimalDNG embeds a plain left-to-right gradient: a real-looking frame to the junk
// filter (a blank one would be flagged and never reach the model).
func minimalDNG(t *testing.T, path string) {
	dngWith(t, path, gradientImage(1600, 1067))
}

func gradientImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(40 + x*160/w)
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
		}
	}
	return img
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
 "composition":{"score":6,"status":"croppable","issues":[],"crop":{"apply":true,"left":0.1,"top":0,"right":1,"bottom":1}},
 "notes":"","keywords":["portrait","forest"]}`, f.status)
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
		if !strings.Contains(string(x), `crs:Exposure2012="+0.50"`) || !strings.Contains(string(x), "<rdf:li>cull:keep</rdf:li>") {
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
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "-o for a separate report") {
		t.Fatalf("backend mismatch: want refusal, got %v", err)
	}
}

// Dropping --resume is no longer an escape (the overwrite guard refuses it, and
// --fresh would pay for the shoot again): a model mismatch names the flags that
// continue the report.
func TestResumeModelMismatchNamesTheReportsModel(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model = "anthropic", "claude-sonnet-5"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	c.Resume, c.Model = true, "claude-sonnet-5-5"
	_, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err == nil || !strings.Contains(err.Error(), `--model "claude-sonnet-5"`) || strings.Contains(err.Error(), "drop --resume") {
		t.Fatalf("got %v", err)
	}
}

// A report from an older schema (e.g. saved before sequence ranking) advises the
// free fix (cull decide upgrades it in place), not dropping --resume: that would
// re-judge, and pay for, the whole shoot again.
func TestResumeOnOlderSchemaAdvisesDecideNotRejudging(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model = "openai", "local-model"
	os.WriteFile(c.ReportPath, []byte(`{"schema_version":1,"model":"local-model","backend":"openai","results":[]}`), 0o644)
	c.Resume = true
	_, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "cull decide") || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("want advice to run cull decide then --resume, got %v", err)
	}
	if strings.Contains(err.Error(), "drop --resume") {
		t.Fatalf("must not advise dropping --resume (that re-judges and pays again): %v", err)
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
			if c.detect != nil && c.wantSource != "face" && ft.FaceQ != 30 {
				t.Fatalf("best sub-threshold face score not recorded: face_q=%v", ft.FaceQ)
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

func TestReportOnDisk(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model, c.WriteXMP = "openai", "local-model", false
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	if _, _, err := Run(context.Background(), c, &routedBackend{locate: `{"confident":false,"kind":"none","subject":"","box":{"left":0,"top":0,"right":1,"bottom":1}}`}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.ReportPath)
	for _, want := range []string{fmt.Sprintf(`"schema_version": %d`, report.SchemaVersion), `"backend": "openai"`, `"focus_target"`, `"source": "none"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report lacks %s", want)
		}
	}
}

func TestRunRecordsLook(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, false
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if g, ok := r.LookBytes(); !ok || len(g) != 192 {
			t.Fatalf("%s: look %v %v", r.File, g, ok)
		}
	}
}

func TestSaveInputsRecordsExactlyWhatIsSent(t *testing.T) {
	for _, dry := range []bool{false, true} {
		dir, out := t.TempDir(), t.TempDir()
		texturedDNG(t, filepath.Join(dir, "L1000001.DNG"))
		c := cfg(dir)
		c.WriteXMP, c.FaceMinQ, c.DryRun, c.SaveInputs = false, 80, dry, out
		c.LandedWithSubject = true // record every kind of part
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

func TestStatsTextIncludesShootingContext(t *testing.T) {
	f, err := imageprep.Decode(func() []byte {
		var b bytes.Buffer
		jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 64, 32)), nil)
		return b.Bytes()
	}(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ex := &dng.Exif{ExposureTime: 1.0 / 125, FNumber: 4.8, FNumberEstimated: true, ISO: 400, FocalLength: 35}
	got := statsText(f, imageprep.Measure(f), ex, nil)
	if !strings.Contains(got, "shooting: 1/125 s, ~f/4.8 (camera estimate), ISO 400, 35 mm") {
		t.Fatalf("stats text: %s", got)
	}
	if got := statsText(f, imageprep.Measure(f), nil, nil); strings.Contains(got, "shooting") {
		t.Fatalf("no exif should add no shooting line: %s", got)
	}
}

func TestBudgetStopKeepsResultsAndRecordsCost(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Concurrency, c.WriteXMP = 1, false
	c.Price = &llm.Price{In: 10_000} // fakeBackend uses 100 input tokens per call: $1 a frame
	c.MaxCost = 2
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("want ErrBudget, got %v", err)
	}
	if len(rep.Results) != 2 || rep.Results[0].CostUSD != 1 {
		t.Fatalf("results=%d cost=%v", len(rep.Results), rep.Results[0].CostUSD)
	}
}

func TestResumeAfterScanSaysSo(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.DryRun = true
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	c.DryRun, c.Resume, c.Backend, c.Model = false, true, "anthropic", "claude-sonnet-5"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "a scan report") {
		t.Fatalf("got %v", err)
	}
}

func TestLandedTileOnlyWithoutSubjectByDefault(t *testing.T) {
	face := func(*imageprep.Frame) []focus.Face {
		return []focus.Face{{Rect: image.Rect(400, 300, 600, 500), Q: 120}}
	}
	for _, c := range []struct {
		name        string
		detect      func(*imageprep.Frame) []focus.Face
		withSubject bool
		wantLanded  bool
	}{
		{"face, default: no landed tile", face, false, false},
		{"face, opted in: landed tile", face, true, true},
		{"no face: landed tile", func(*imageprep.Frame) []focus.Face { return nil }, false, true},
	} {
		dir := t.TempDir()
		texturedDNG(t, filepath.Join(dir, "L1000001.DNG"))
		conf := cfg(dir)
		conf.WriteXMP, conf.FaceMinQ, conf.detect, conf.LandedWithSubject = false, 80, c.detect, c.withSubject
		rb := &routedBackend{}
		if _, _, err := Run(context.Background(), conf, rb); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(requestText(rb.evalReq), "where focus most likely landed"); got != c.wantLanded {
			t.Errorf("%s: landed tile sent = %v", c.name, got)
		}
	}
}

func TestRerunWithoutResumeRefusesPaidReport(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "r.json"))
	for _, dry := range []bool{false, true} { // judge, then scan
		c := cfg(dir)
		c.DryRun = dry
		_, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
		if err == nil || !strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), "--fresh") {
			t.Fatalf("dry=%v: want a refusal naming --resume and --fresh, got %v", dry, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "r.json")); !bytes.Equal(before, after) {
		t.Fatal("report changed on disk")
	}
	c := cfg(dir)
	c.Fresh = true
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatalf("--fresh: %v", err)
	}
}

func TestFreshRunOverwritesScanReport(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.DryRun = true
	for i := 0; i < 2; i++ { // scan twice: nothing paid, no refusal
		if _, _, err := Run(context.Background(), c, nil); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatalf("judge over a scan report: %v", err)
	}
}

func TestResumeKeepsCostOfDiscardedResults(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Concurrency, c.WriteXMP = 1, false
	c.Price = &llm.Price{In: 10_000} // $1 per call of 100 input tokens
	// the second call fails after being billed: its usage rides on the error
	if _, _, err := Run(context.Background(), c, &billedFailBackend{failOn: 2}); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Cost(); got != 5 { // 4 frames + 1 discarded failure
		t.Fatalf("cost %v, want 5", got)
	}
}

// billedFailBackend answers like fakeBackend but fails call failOn with usage attached.
type billedFailBackend struct{ n, failOn int32 }

func (b *billedFailBackend) Name() string { return "fake" }
func (b *billedFailBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if atomic.AddInt32(&b.n, 1) == b.failOn {
		return &llm.Response{Usage: llm.Usage{InputTokens: 100}}, errors.New("model output does not match schema")
	}
	return (&fakeBackend{status: "sharp"}).Call(ctx, req)
}

func TestResumeAfterFolderRenameRejudgesNothing(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "shoot")
	os.Mkdir(old, 0o755)
	minimalDNG(t, filepath.Join(old, "L1000001.DNG"))
	if _, _, err := Run(context.Background(), cfg(old), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(parent, "2026-10-04 shoot")
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	c := cfg(renamed)
	c.Resume = true
	rep, usage, err := Run(context.Background(), c, &fakeBackend{status: "missed_focus"})
	if err != nil || usage.InputTokens != 0 || len(rep.Results) != 1 || !strings.HasPrefix(rep.Results[0].File, renamed) {
		t.Fatalf("err=%v usage=%+v results=%+v", err, usage, rep.Results)
	}
}

// A file changed since it was judged is judged again, and replaces its old result.
func TestResumeReplacesResultOfChangedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "L1000001.DNG")
	minimalDNG(t, p)
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, time.Now(), time.Now().Add(time.Hour))
	c := cfg(dir)
	c.Resume = true
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil || len(rep.Results) != 1 {
		t.Fatalf("err=%v results=%d", err, len(rep.Results))
	}
}

// A doubtful frame (soft or worse) is asked again; when the second answer says
// sharp, the frame goes to review rather than trusting either. A sharp frame
// is not asked twice.
func TestSecondOpinionOnDoubtfulFrames(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.DNG"))
	c := cfg(dir)
	c.WriteXMP, c.SecondOpinion = false, true
	b := &alternatingBackend{first: map[string]string{"L1000001": "missed_focus", "L1000002": "sharp"}, then: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := result(t, rep, "L1000001.DNG"), result(t, rep, "L1000002.DNG")
	if r1.Second == nil || r1.Decision != eval.Review || r2.Second != nil {
		t.Fatalf("r1 second=%v decision=%s; r2 second=%v", r1.Second, r1.Decision, r2.Second)
	}
	if r1.Usage.InputTokens != 2*r2.Usage.InputTokens {
		t.Fatalf("second opinion's usage not counted: %d vs %d", r1.Usage.InputTokens, r2.Usage.InputTokens)
	}
}

// alternatingBackend answers a file's first evaluation from first, later ones with then.
type alternatingBackend struct {
	mu    sync.Mutex
	first map[string]string
	then  string
	seen  map[string]bool
}

func (a *alternatingBackend) Name() string { return "fake" }
func (a *alternatingBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	a.mu.Lock()
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	status := a.then
	for name, s := range a.first {
		if strings.Contains(req.Parts[0].Text, name) && !a.seen[name] {
			status, a.seen[name] = s, true
		}
	}
	a.mu.Unlock()
	return (&fakeBackend{status: status}).Call(ctx, req)
}

// Of two confident faces, the larger is the subject: Q measures how frontal a face
// is, not how important.
func TestSubjectIsTheLargestConfidentFace(t *testing.T) {
	small := focus.Face{Rect: image.Rect(10, 10, 60, 60), Q: 200}
	large := focus.Face{Rect: image.Rect(300, 200, 600, 500), Q: 150}
	c := cfg(t.TempDir())
	c.FaceMinQ = 80
	c.detect = func(*imageprep.Frame) []focus.Face { return []focus.Face{small, large} }
	ft := &report.FocusTarget{}
	frame := &imageprep.Frame{W: 1600, H: 1067}
	if _, need := faceTarget(c, frame, ft); need || ft.Box == nil || ft.Box.Left < 0.18 || ft.FaceQ != 200 {
		t.Fatalf("box %+v faceQ %v", ft.Box, ft.FaceQ)
	}
}

// Scan records the advisory eye measure when the face's pupils were found.
func TestScanRecordsEyeDetail(t *testing.T) {
	for _, c := range []struct {
		name   string
		pupils []image.Point
		want   bool
	}{{"pupils", []image.Point{{460, 380}, {540, 380}}, true}, {"no pupils", nil, false}} {
		dir := t.TempDir()
		texturedDNG(t, filepath.Join(dir, "L1000001.DNG"))
		conf := cfg(dir)
		conf.DryRun, conf.WriteXMP, conf.FaceMinQ = true, false, 80
		conf.detect = func(*imageprep.Frame) []focus.Face {
			return []focus.Face{{Rect: image.Rect(400, 300, 600, 500), Q: 120, Pupils: c.pupils}}
		}
		rep, _, err := Run(context.Background(), conf, nil)
		if err != nil {
			t.Fatal(err)
		}
		ft := rep.Results[0].FocusTarget
		if got := ft.EyeSharpness > 0 && ft.FaceSharpness > 0; got != c.want {
			t.Errorf("%s: eye %v face %v", c.name, ft.EyeSharpness, ft.FaceSharpness)
		}
	}
}

// A larger face barely over --face-min-q (where false positives live) doesn't
// displace a much more certain one.
func TestLargeWeakFaceDoesNotWin(t *testing.T) {
	sure := focus.Face{Rect: image.Rect(10, 10, 60, 60), Q: 400}
	weak := focus.Face{Rect: image.Rect(300, 200, 600, 500), Q: 90}
	c := cfg(t.TempDir())
	c.FaceMinQ = 80
	c.detect = func(*imageprep.Frame) []focus.Face { return []focus.Face{sure, weak} }
	ft := &report.FocusTarget{}
	if _, _ = faceTarget(c, &imageprep.Frame{W: 1600, H: 1067}, ft); ft.Box == nil || ft.Box.Left > 0.05 {
		t.Fatalf("box %+v", ft.Box)
	}
}

// Cache writes and reads are priced, not dropped, when a frame's cost is computed.
func TestFrameCostIncludesCacheTokens(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.WriteXMP = false
	c.Price = &llm.Price{In: 10_000, Out: 0, Read: 1_000} // $1 per 100 input; cache read $0.10 per 100
	rep, _, err := Run(context.Background(), c, &cachedBackend{})
	if err != nil {
		t.Fatal(err)
	}
	// 100 uncached ($1) + 100 written (1.25 × $1) + 100 read ($0.10)
	if got := rep.Results[0].CostUSD; math.Abs(got-2.35) > 1e-9 {
		t.Fatalf("cost %v", got)
	}
}

type cachedBackend struct{ fakeBackend }

func (c *cachedBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	r, err := (&fakeBackend{status: "sharp"}).Call(ctx, req)
	if r != nil {
		r.Usage = llm.Usage{InputTokens: 100, CacheWriteTokens: 100, CacheReadTokens: 100}
	}
	return r, err
}

// effortRecorder answers like fakeBackend and records each call's schema and effort.
type effortRecorder struct {
	mu   sync.Mutex
	seen map[string]string // schema name -> effort
}

func (e *effortRecorder) Name() string { return "fake" }
func (e *effortRecorder) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	e.mu.Lock()
	if e.seen == nil {
		e.seen = map[string]string{}
	}
	e.seen[req.SchemaName] = req.Effort
	e.mu.Unlock()
	return (&fakeBackend{status: "sharp"}).Call(ctx, req)
}

func TestEffortReachesEachCallAndTheReport(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.WriteXMP, c.Locate, c.Effort, c.LocateEffort = false, true, "medium", "low"
	c.detect = func(*imageprep.Frame) []focus.Face { return nil } // no face: locate runs
	b := &effortRecorder{}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if b.seen["focus_target"] != "low" || b.seen["evaluation"] != "medium" || rep.Effort != "medium" || rep.LocateEffort != "low" {
		t.Fatalf("seen %v report %q/%q", b.seen, rep.Effort, rep.LocateEffort)
	}
}

func TestResumeRefusesDifferentEffort(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	c := cfg(dir)
	c.Resume, c.Effort = true, "low"
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("got %v", err)
	}
	c.Effort = ""
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatalf("same (default) effort: %v", err)
	}
}

// pinnedBackend is a fake whose model name is an alias resolved per call.
type pinnedBackend struct {
	fakeBackend
	resolves, pinned string
}

func (p *pinnedBackend) Resolved() string { return p.resolves }
func (p *pinnedBackend) Pin(model string) { p.pinned = model }

func TestRunRecordsAndPinsTheResolvedModel(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	b := &pinnedBackend{fakeBackend: fakeBackend{status: "sharp"}, resolves: "claude-sonnet-5"}
	rep, _, err := Run(context.Background(), cfg(dir), b)
	if err != nil || rep.ResolvedModel != "claude-sonnet-5" || b.pinned != "" {
		t.Fatalf("err=%v resolved=%q pinned=%q", err, rep.ResolvedModel, b.pinned)
	}
	c := cfg(dir)
	c.Resume = true
	b2 := &pinnedBackend{fakeBackend: fakeBackend{status: "sharp"}, resolves: "claude-sonnet-5"}
	if _, _, err := Run(context.Background(), c, b2); err != nil || b2.pinned != "claude-sonnet-5" {
		t.Fatalf("resume didn't pin: err=%v pinned=%q", err, b2.pinned)
	}
}

// When an alias has moved on, the abort says to rerun with the model the report
// resolved to: resume must accept exactly that.
func TestResumeAcceptsTheResolvedModelName(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Backend, c.Model = "claude-code", "sonnet"
	b := &pinnedBackend{fakeBackend: fakeBackend{status: "sharp"}, resolves: "claude-sonnet-5"}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	c.Resume, c.Model = true, "claude-sonnet-5"
	rep, _, err := Run(context.Background(), c, &pinnedBackend{fakeBackend: fakeBackend{status: "sharp"}, resolves: "claude-sonnet-5"})
	if err != nil || rep.Model != "sonnet" {
		t.Fatalf("err=%v model=%q", err, rep.Model)
	}
}

// Batch requests carry the run's effort like sync calls do.
func TestBatchRequestsCarryEffort(t *testing.T) {
	_, c := batchShoot(t)
	c.Effort, c.LocateEffort = "medium", "low"
	fb := &fakeBatch{locate: map[string]string{locatePrompt: `{"confident":true,"kind":"eye","subject":"eye","box":{"left":0.4,"top":0.3,"right":0.45,"bottom":0.35}}`}}
	if _, _, err := RunBatch(context.Background(), c, fb); err != nil {
		t.Fatal(err)
	}
	for _, batch := range fb.submitted {
		for _, r := range batch {
			want := "medium"
			if r.Req.SchemaName == "focus_target" {
				want = "low"
			}
			if r.Req.Effort != want {
				t.Fatalf("%s: effort %q, want %q", r.CustomID, r.Req.Effort, want)
			}
		}
	}
}

// cull rank pins the alias to what the report was judged with.
func TestRankPinsTheReportsResolvedModel(t *testing.T) {
	c, rep := seqShoot(t, 3)
	rep.ResolvedModel = "claude-sonnet-5"
	if err := rep.Save(c.ReportPath); err != nil {
		t.Fatal(err)
	}
	b := &pinnedRankBackend{rankBackend: rankBackend{order: reverse}}
	if _, err := Rank(context.Background(), c, b, false); err != nil {
		t.Fatal(err)
	}
	if b.pinned != "claude-sonnet-5" {
		t.Fatalf("pinned %q", b.pinned)
	}
}

type pinnedRankBackend struct {
	rankBackend
	pinned string
}

func (p *pinnedRankBackend) Resolved() string { return "claude-sonnet-5" }
func (p *pinnedRankBackend) Pin(model string) { p.pinned = model }
