package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
)

const evalOK = `{"sharpness":{"score":8,"status":"sharp","focus_target":""},
 "exposure":{"score":7,"status":"good","ev_adjust":0,"clipping":"none","reason":""},
 "composition":{"score":6,"status":"good","issues":[],"crop":{"apply":false,"left":0,"top":0,"right":1,"bottom":1},"straighten_degrees":0},
 "people":{"present":true,"eyes":"open","expression":"good"},"notes":""}`

// fakeBatch is an in-memory Message Batches service.
type fakeBatch struct {
	mu          sync.Mutex
	submitted   [][]llm.BatchRequest
	locate      map[string]string // file base name -> locate JSON; missing = errored item
	statusErr   error
	statusErrID string            // with statusErr: only this batch fails its status check ("" = every batch)
	hold        bool              // batches stay in progress
	holdID      string            // with this set: only this batch ID stays in progress; others end normally regardless of hold
	onStatus    func(id string)   // called on every status check
	resultsErr  error             // BatchResults fails with this
	rankOrder   func(n int) []int // rank answers, 1-based, best first; nil = reverse capture order
	calls       int
	failOn      int   // SubmitBatch call number that fails (0 = none)
	failErr     error // with this error
}

func (f *fakeBatch) SubmitBatch(_ context.Context, reqs []llm.BatchRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == f.failOn {
		return "", f.failErr
	}
	f.submitted = append(f.submitted, reqs)
	return fmt.Sprintf("b%d", len(f.submitted)), nil
}

func (f *fakeBatch) BatchStatus(_ context.Context, id string) (llm.BatchStatus, error) {
	if f.onStatus != nil {
		f.onStatus(id)
	}
	if f.statusErr != nil && (f.statusErrID == "" || f.statusErrID == id) {
		return llm.BatchStatus{}, f.statusErr
	}
	ended := !f.hold
	if f.holdID != "" {
		ended = id != f.holdID
	}
	return llm.BatchStatus{ID: id, Ended: ended, ResultsURL: id}, nil
}

func (f *fakeBatch) BatchResults(_ context.Context, id string, schemaFor func(string) map[string]any, fn func(llm.BatchResult)) error {
	if f.resultsErr != nil {
		return f.resultsErr
	}
	f.mu.Lock()
	var n int
	fmt.Sscanf(id, "b%d", &n)
	reqs := f.submitted[n-1]
	f.mu.Unlock()
	for i := len(reqs) - 1; i >= 0; i-- { // any order
		r := reqs[i]
		res := llm.BatchResult{CustomID: r.CustomID}
		file := strings.TrimPrefix(r.Req.Parts[0].Text, "File: ")
		file = strings.SplitN(file, "\n", 2)[0]
		switch {
		case r.Req.SchemaName == "ranking":
			n := 0
			for _, p := range r.Req.Parts {
				if strings.HasSuffix(p.Text, ": full frame") {
					n++
				}
			}
			order := reverse(n)
			if f.rankOrder != nil {
				order = f.rankOrder(n)
			}
			var entries []string
			for _, k := range order {
				entries = append(entries, fmt.Sprintf(`{"frame":%d,"strength":"s","weakness":"w"}`, k))
			}
			res.Response = &llm.Response{JSON: json.RawMessage(`{"ranking":[` + strings.Join(entries, ",") + `],"summary":"batch best"}`),
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 100}}
			if err := llm.Validate(schemaFor(r.CustomID), res.Response.JSON); err != nil { // as the real client does
				res.Err = fmt.Errorf("model output does not match schema: %w", err)
			}
		case strings.HasPrefix(r.CustomID, "E-"):
			res.Response = &llm.Response{JSON: json.RawMessage(evalOK), Usage: llm.Usage{InputTokens: 100}}
		case f.locate[r.Req.Parts[len(r.Req.Parts)-2].Text] != "":
			res.Response = &llm.Response{JSON: json.RawMessage(f.locate[r.Req.Parts[len(r.Req.Parts)-2].Text]), Usage: llm.Usage{InputTokens: 50}}
		default:
			res.Err = errors.New("batch item errored: overloaded_error: busy")
		}
		_ = file
		fn(res)
	}
	return nil
}

func (f *fakeBatch) ids(round int) []string {
	var ids []string
	for _, r := range f.submitted[round] {
		ids = append(ids, r.CustomID[:2])
	}
	sort.Strings(ids)
	return ids
}

// batchShoot: three frames told apart by width; only the 1600-wide one has a face.
func batchShoot(t *testing.T) (string, Config) {
	t.Helper()
	dir := t.TempDir()
	for i, w := range []int{1600, 1601, 1602} {
		dngWith(t, filepath.Join(dir, fmt.Sprintf("L100000%d.DNG", i+1)), image.NewRGBA(image.Rect(0, 0, w, 1067)))
	}
	c := cfg(dir)
	c.WriteXMP, c.Locate, c.FaceMinQ, c.Backend, c.Model = false, true, 80, "anthropic", "claude-sonnet-5"
	c.Batch, c.Price = true, &llm.Price{In: 10_000}
	c.detect = func(f *imageprep.Frame) []focus.Face {
		if f.W == 1600 {
			return []focus.Face{{Rect: image.Rect(400, 300, 600, 500), Q: 120}}
		}
		return nil
	}
	return dir, c
}

// every locate request carries the same prompt text; key the fake's answers by it.
const locatePrompt = "Find the intended focus target in this photograph:"

func TestBatchTwoRoundsLocateThenEvaluate(t *testing.T) {
	_, c := batchShoot(t)
	fb := &fakeBatch{locate: map[string]string{
		locatePrompt: `{"confident":true,"kind":"eye","subject":"eye","box":{"left":0.4,"top":0.3,"right":0.45,"bottom":0.35}}`,
	}}
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || strings.Join(fb.ids(0), " ") != "E- L- L-" || strings.Join(fb.ids(1), " ") != "E- E-" {
		t.Fatalf("batches: %d, round1 %v, round2 %v", len(fb.submitted), fb.ids(0), fb.ids(1))
	}
	if len(rep.Results) != 3 {
		t.Fatalf("results %d", len(rep.Results))
	}
	for _, r := range rep.Results {
		if r.Error != "" || r.Evaluation == nil || r.Decision != eval.Keep {
			t.Fatalf("%s: error=%q decision=%s", r.File, r.Error, r.Decision)
		}
	}
	if r := result(t, rep, "L1000002.DNG"); r.FocusTarget.Source != "model" || r.CostUSD != 0.75 { // (50+100) tokens at half price
		t.Fatalf("L2: focus=%+v cost=%v", r.FocusTarget, r.CostUSD)
	}
	if _, err := os.Stat(c.ReportPath + ".batch.json"); err == nil {
		t.Fatal("state file left behind after a completed run")
	}
}

func TestBatchReattachesInsteadOfResubmitting(t *testing.T) {
	_, c := batchShoot(t)
	fb := &fakeBatch{statusErr: errors.New("network down")}
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil {
		t.Fatal("expected the first run to fail while polling")
	}
	if len(fb.submitted) != 1 {
		t.Fatalf("submitted %d", len(fb.submitted))
	}
	fb.statusErr = nil
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("a rerun without --resume must refuse, got %v", err)
	}
	c.Resume = true
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(rep.Results) != 3 { // round 1 re-attached, only round 2 submitted
		t.Fatalf("submitted %d batches, %d results", len(fb.submitted), len(rep.Results))
	}
	if r := result(t, rep, "L1000003.DNG"); !strings.Contains(r.FocusTarget.Reason, "locate failed") || r.Evaluation == nil {
		t.Fatalf("errored locate item: %+v", r.FocusTarget)
	}
}

func TestBatchRefusesInterruptedSubmission(t *testing.T) {
	_, c := batchShoot(t)
	os.WriteFile(c.ReportPath+".batch.json", []byte(`{"version":1,"backend":"anthropic","model":"claude-sonnet-5","frames":{},"batches":[{"round":1,"status":"submitting"}]}`), 0o644)
	c.Resume = true
	fb := &fakeBatch{}
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil || !strings.Contains(err.Error(), "not resubmitting") || len(fb.submitted) != 0 {
		t.Fatalf("err=%v submitted=%d", err, len(fb.submitted))
	}
}

func TestBatchChecksBudgetBeforeSubmitting(t *testing.T) {
	_, c := batchShoot(t)
	c.Price, c.MaxCost = &llm.Price{In: 2, Out: 10}, 0.001
	fb := &fakeBatch{}
	if _, _, err := RunBatch(context.Background(), c, fb); !errors.Is(err, llm.ErrBudget) || len(fb.submitted) != 0 {
		t.Fatalf("err=%v submitted=%d", err, len(fb.submitted))
	}
}

func TestBatchChunksLargeRounds(t *testing.T) {
	_, c := batchShoot(t)
	c.BatchChunkBytes = 1 // every request in its own batch
	fb := &fakeBatch{locate: map[string]string{locatePrompt: `{"confident":false,"kind":"none","subject":"","box":{"left":0,"top":0,"right":1,"bottom":1}}`}}
	if _, _, err := RunBatch(context.Background(), c, fb); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 5 {
		t.Fatalf("submitted %d batches, want 5 (3 + 2)", len(fb.submitted))
	}
}

func TestSyncRunRefusesWhileBatchStateExists(t *testing.T) {
	_, c := batchShoot(t)
	os.WriteFile(c.ReportPath+".batch.json", []byte(`{"version":1}`), 0o644)
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err == nil || !strings.Contains(err.Error(), "batch") {
		t.Fatalf("got %v", err)
	}
	_ = report.SchemaVersion
}

func TestBatchRound2WithManyFramesIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 16; i++ {
		dngWith(t, filepath.Join(dir, fmt.Sprintf("L10000%02d.DNG", i)), image.NewRGBA(image.Rect(0, 0, 1601+i, 1067)))
	}
	c := cfg(dir)
	c.WriteXMP, c.Locate, c.FaceMinQ, c.Backend, c.Model, c.Concurrency = false, true, 80, "anthropic", "claude-sonnet-5", 8
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	fb := &fakeBatch{locate: map[string]string{locatePrompt: `{"confident":true,"kind":"eye","subject":"eye","box":{"left":0.4,"top":0.3,"right":0.45,"bottom":0.35}}`}}
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil || len(rep.Results) != 16 {
		t.Fatalf("err=%v results=%d", err, len(rep.Results))
	}
}

func TestBatchAmbiguousSubmitFailureIsNeverResubmitted(t *testing.T) {
	_, c := batchShoot(t)
	fb := &fakeBatch{failOn: 1, failErr: context.Canceled} // Ctrl-C mid-upload: the batch may exist
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil {
		t.Fatal("expected the interrupted submit to fail")
	}
	c.Resume = true
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil || !strings.Contains(err.Error(), "not resubmitting") {
		t.Fatalf("resume after an ambiguous submit must refuse, got %v", err)
	}
	if len(fb.submitted) != 0 {
		t.Fatalf("resubmitted %d batches", len(fb.submitted))
	}
}

func TestBatchRejectedSubmitIsReleasedAndStrandedFramesAreSent(t *testing.T) {
	_, c := batchShoot(t)
	c.BatchChunkBytes = 1 // one request per batch
	fb := &fakeBatch{failOn: 2, failErr: fmt.Errorf("%w: api status 400: too large", llm.ErrRejected),
		locate: map[string]string{locatePrompt: `{"confident":false,"kind":"none","subject":"","box":{"left":0,"top":0,"right":1,"bottom":1}}`}}
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil {
		t.Fatal("expected the rejected submit to fail the run")
	}
	c.Resume = true
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if r.Error != "" || r.Evaluation == nil {
			t.Fatalf("%s stranded: %q", r.File, r.Error)
		}
	}
	if len(rep.Results) != 3 {
		t.Fatalf("results %d", len(rep.Results))
	}
}
