package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
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
	if f.statusErr != nil && (f.statusErrID == "" || f.statusErrID == id) {
		return llm.BatchStatus{}, f.statusErr
	}
	return llm.BatchStatus{ID: id, Ended: true, ResultsURL: id}, nil
}

func (f *fakeBatch) BatchResults(_ context.Context, id string, schemaFor func(string) map[string]any, fn func(llm.BatchResult)) error {
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

func TestRankBatchRoundsAndCost(t *testing.T) {
	c, rep := seqShoot(t, 9)
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.Batch = &p, true
	fb := &fakeBatch{}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(fb.submitted[0]) != 2 || len(fb.submitted[1]) != 1 {
		t.Fatalf("want round 1 = 2 chunk requests, round 2 = 1 final; got %d batches", len(fb.submitted))
	}
	s := rep.Sets[0]
	if s.By != "model" || len(s.Order) != 9 || s.Summary != "batch best" {
		t.Fatalf("set %+v", s)
	}
	if want := p.Cost(s.Usage, true); s.CostUSD != want {
		t.Fatalf("batch price: %v, want %v", s.CostUSD, want)
	}
	if _, err := os.Stat(ex.statePath); err == nil {
		t.Fatal("state file left behind after ranking finished")
	}
}

func TestRankBatchReattaches(t *testing.T) {
	c, rep := seqShoot(t, 3)
	c.Batch = true
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	if _, err := os.Stat(ex.statePath); err != nil {
		t.Fatal("the submitted batch must be recorded before polling")
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 {
		t.Fatalf("the re-run resubmitted: %d batches", len(fb.submitted))
	}
	if rep.Sets[0].By != "model" {
		t.Fatal("not ranked after re-attaching")
	}
}

// An answer that isn't a clean order fails its set (it stays by scores) and is
// paid for; a batch has no retry, so nothing is resubmitted.
func TestRankBatchNonPermutationLeavesSetUnranked(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{rankOrder: func(int) []int { return []int{1, 1, 2} }}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatalf("a failed set is not a stop: %v", err)
	}
	if s := rep.Sets[0]; s.By == "model" || len(s.Order) != 0 || s.Usage.InputTokens != 1000 || len(fb.submitted) != 1 {
		t.Fatalf("set %+v after %d batches", s, len(fb.submitted))
	}
	if exists(ex.statePath) {
		t.Fatal("state file left behind after ranking finished")
	}
}

// nineAndTwo: seqShoot(11) regrouped as {L1..L9}, ranked in two chunk calls and a
// final, and {L10, L11}, ranked in one call.
func nineAndTwo(t *testing.T) (Config, *report.Report) {
	t.Helper()
	c, rep := seqShoot(t, 11)
	bright := make([]uint8, 192) // upper half bright: far from the textured look
	for i := range bright[:96] {
		bright[i] = 255
	}
	rep.Results[9].Look, rep.Results[10].Look = report.EncodeLook(bright), report.EncodeLook(bright)
	decideAll(rep, c.Policy, c.Seq)
	if len(rep.Sets) != 2 || rep.Sets[0].Of != 9 || rep.Sets[1].Of != 2 {
		t.Fatalf("setup: sets %+v", rep.Sets)
	}
	return c, rep
}

// When round 2 is left pending, no set of the wave is ranked, so the re-run sends
// the same round 1: it is answered from the state (not paid again) and round 2 is
// re-attached. Every answer is charged once.
func TestRankBatchRound2FailureReplaysRound1(t *testing.T) {
	c, rep := nineAndTwo(t)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // the finals
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("round 2's status failure must surface")
	}
	for _, s := range rep.Sets {
		if s.By == "model" {
			t.Fatalf("set %d ranked from a wave left pending", s.ID)
		}
	}
	if in := rep.Sets[0].Usage.InputTokens + rep.Sets[1].Usage.InputTokens; in != 3000 || !exists(ex.statePath) {
		t.Fatalf("round 1 (3 calls) is paid: %d input tokens; state kept: %v", in, exists(ex.statePath))
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 {
		t.Fatalf("the re-run resubmitted: %d batches", len(fb.submitted))
	}
	if rep.Sets[0].By != "model" || len(rep.Sets[0].Order) != 9 || rep.Sets[1].By != "model" {
		t.Fatalf("sets %+v", rep.Sets)
	}
	all := llm.Usage{InputTokens: 4000, OutputTokens: 400}
	if in := rep.Sets[0].Usage.InputTokens + rep.Sets[1].Usage.InputTokens; in != all.InputTokens ||
		math.Abs(rep.RankCostUSD-p.Cost(all, true)) > 1e-12 {
		t.Fatalf("each answer charged once: %d input tokens, rank cost %v, want %v", in, rep.RankCostUSD, p.Cost(all, true))
	}
	if exists(ex.statePath) {
		t.Fatal("state file left behind after ranking finished")
	}
}

// A round too big for one upload goes out in several batches. When one of them
// fails, nothing is handed over; the re-run re-attaches to all of them and each
// answer is charged once.
func TestRankBatchSplitsLargeRounds(t *testing.T) {
	c, rep := seqShoot(t, 9)
	c.BatchChunkBytes = 1                                                      // every request in its own batch
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // round 1's second batch
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	if len(fb.submitted) != 2 || rep.Sets[0].Usage.InputTokens != 0 {
		t.Fatalf("round 1 in 2 batches, none handed over: %d batches, usage %+v", len(fb.submitted), rep.Sets[0].Usage)
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if s := rep.Sets[0]; len(fb.submitted) != 3 || s.By != "model" || s.Usage.InputTokens != 3000 {
		t.Fatalf("%d batches (want 2 re-attached + the final), set %+v", len(fb.submitted), s)
	}
}

// A submission cut off before its batch ID was recorded may have created the batch:
// a re-run refuses rather than pay twice.
func TestRankBatchRefusesInterruptedSubmission(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{failOn: 1, failErr: context.Canceled} // Ctrl-C mid-upload
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("expected the interrupted submit to fail")
	}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil || !strings.Contains(err.Error(), "not resubmitting") || len(fb.submitted) != 0 {
		t.Fatalf("err=%v submitted=%d", err, len(fb.submitted))
	}
}

// Pricing and wave shape follow the executor, not cfg.Batch: a sync ranking under
// cfg.Batch is billed at the sync rate and checks the budget between waves of
// cfg.Concurrency sets.
func TestRankSyncExecIgnoresCfgBatch(t *testing.T) {
	c, rep := twoSets(t)
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.Batch, c.Concurrency, c.MaxCost = &p, true, 1, 1e-9
	b := &rankBackend{order: reverse}
	err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false)
	if !errors.Is(err, llm.ErrBudget) || b.calls != 1 || rep.Sets[1].By == "model" {
		t.Fatalf("one set per wave: err %v, calls %d, sets %+v", err, b.calls, rep.Sets)
	}
	if s := rep.Sets[0]; s.By != "model" || s.CostUSD != p.Cost(s.Usage, false) {
		t.Fatalf("sync price: %v, want %v", s.CostUSD, p.Cost(s.Usage, false))
	}
}

// While a batch ranking is recorded, ranking without a batch refuses: it would pay
// for the same sets again.
func TestSyncRankRefusesWhileRankBatchPending(t *testing.T) {
	c, rep := seqShoot(t, 3)
	if err := os.WriteFile(c.ReportPath+".rank-batch.json", []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &rankBackend{order: reverse}
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false); err == nil || !strings.Contains(err.Error(), "--batch") || b.calls != 0 {
		t.Fatalf("err %v, %d calls", err, b.calls)
	}
}

// rankShoot: four identical textured frames (one set), judged and ranked in batches.
func rankShoot(t *testing.T) (Config, llm.Price) {
	t.Helper()
	dir := t.TempDir()
	for i := 1; i <= 4; i++ {
		texturedDNG(t, filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i)))
	}
	c := cfg(dir)
	c.WriteXMP, c.Backend, c.Model, c.Batch, c.Rank = false, "anthropic", "claude-sonnet-5", true, true
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	c.Policy.KeepBest, c.Policy.Outranked = 1, eval.ActionReview
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	return c, p
}

// judge --batch ranks the sets in one more batch round, at the batch price, and
// decides again with the model's order.
func TestBatchJudgeRanksSetsInABatch(t *testing.T) {
	c, p := rankShoot(t)
	fb := &fakeBatch{}
	rep, usage, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(fb.submitted[1]) != 1 || fb.submitted[1][0].CustomID != "S1" {
		t.Fatalf("want the judge batch, then one rank request: %d batches", len(fb.submitted))
	}
	if s := rep.Sets[0]; s.By != "model" || filepath.Base(s.Order[0]) != "L0000004.DNG" {
		t.Fatalf("set %+v", s)
	}
	// By scores L1 was best and L4 outranked; the model reverses that.
	if l1, l4 := result(t, rep, "L0000001.DNG"), result(t, rep, "L0000004.DNG"); l1.Decision != eval.Review || l4.Decision != eval.Keep {
		t.Fatalf("L1 %s, L4 %s", l1.Decision, l4.Decision)
	}
	if want := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, true); rep.RankCostUSD != want {
		t.Fatalf("rank cost %v, want %v", rep.RankCostUSD, want)
	}
	if usage.InputTokens != 4*100+1000 {
		t.Fatalf("this run's usage includes the ranking: %+v", usage)
	}
	if saved, err := report.Load(c.ReportPath); err != nil || saved.Sets[0].By != "model" {
		t.Fatalf("saved: %v", err)
	}
	for _, f := range []string{".batch.json", ".rank-batch.json"} {
		if exists(c.ReportPath + f) {
			t.Fatalf("%s left behind", f)
		}
	}
}

// A ranking batch that fails while polling leaves the judged frames in the saved
// report and no judge state (a resume would add them twice); a rerun needs
// --resume, and then re-attaches to the ranking batch, sending nothing again.
func TestBatchJudgeRankReattachesOnResume(t *testing.T) {
	c, _ := rankShoot(t)
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // the ranking batch
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil {
		t.Fatal("the ranking's status failure must surface")
	}
	saved, err := report.Load(c.ReportPath)
	if err != nil || len(saved.Results) != 4 || exists(c.ReportPath+".batch.json") || !exists(c.ReportPath+".rank-batch.json") {
		t.Fatalf("after the failed ranking: %v, judge state %v, rank state %v", err, exists(c.ReportPath+".batch.json"), exists(c.ReportPath+".rank-batch.json"))
	}
	fb.statusErr = nil
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("a rerun without --resume must refuse (it would judge again), got %v", err)
	}
	c.Resume = true
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(rep.Results) != 4 || rep.Sets[0].By != "model" {
		t.Fatalf("%d batches, %d results, set %+v", len(fb.submitted), len(rep.Results), rep.Sets[0])
	}
	if exists(c.ReportPath + ".rank-batch.json") {
		t.Fatal("rank state left behind")
	}
}
