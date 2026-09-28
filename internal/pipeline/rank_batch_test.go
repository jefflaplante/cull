package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func rankEx(fb *fakeBatch, c Config) batchExec {
	return batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
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
	ex := rankEx(fb, c)
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
	rep.Results[9].Look, rep.Results[10].Look = brightLook(), brightLook()
	decideAll(rep, c.Policy, c.Seq)
	if len(rep.Sets) != 2 || rep.Sets[0].Of != 9 || rep.Sets[1].Of != 2 {
		t.Fatalf("setup: sets %+v", rep.Sets)
	}
	return c, rep
}

// brightLook: upper half bright, far from the textured frames' look.
func brightLook() string {
	b := make([]uint8, 192)
	for i := range b[:96] {
		b[i] = 255
	}
	return report.EncodeLook(b)
}

// When round 2 is left pending, the set answered in one call is ranked and the
// chunked set waits for its final. The re-run reuses the chunk answers (charged
// once) and re-attaches to the final: every answer is charged once.
func TestRankBatchRound2FailureReusesRound1(t *testing.T) {
	c, rep := nineAndTwo(t)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // the finals
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("round 2's status failure must surface")
	}
	if rep.Sets[0].By == "model" || rep.Sets[1].By != "model" {
		t.Fatalf("the chunked set waits for its final, the other is ranked: %+v", rep.Sets)
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
// fails, the answers already in are handed over; the re-run re-attaches to the
// other and each answer is charged once.
func TestRankBatchSplitsLargeRounds(t *testing.T) {
	c, rep := seqShoot(t, 9)
	c.BatchChunkBytes = 1                                                      // every request in its own batch
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // round 1's second batch
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	if len(fb.submitted) != 2 || rep.Sets[0].Usage.InputTokens != 1000 {
		t.Fatalf("round 1 in 2 batches, the first one's answer handed over: %d batches, usage %+v", len(fb.submitted), rep.Sets[0].Usage)
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if s := rep.Sets[0]; len(fb.submitted) != 3 || s.By != "model" || s.Usage.InputTokens != 3000 {
		t.Fatalf("%d batches (want 2 re-attached + the final), set %+v", len(fb.submitted), s)
	}
}

// fiveInTwoSets: seqShoot(5) regrouped as {L1, L2} and {L3, L4, L5}.
func fiveInTwoSets(t *testing.T) (Config, *report.Report) {
	t.Helper()
	c, rep := seqShoot(t, 5)
	for i := 2; i < 5; i++ {
		rep.Results[i].Look = brightLook()
	}
	decideAll(rep, c.Policy, c.Seq)
	if len(rep.Sets) != 2 || rep.Sets[0].Of != 2 || rep.Sets[1].Of != 3 {
		t.Fatalf("setup: sets %+v", rep.Sets)
	}
	return c, rep
}

// The review's resume case: between the interrupted ranking and its re-run, a set
// changes (a frame errors or is judged again). Only the changed set's call is sent
// again; the unchanged set's answer comes from the recorded batch, which is
// collected, not dropped, and the answer no set uses still counts in the cost.
func TestRankBatchReusesAnswersWhenSetsChange(t *testing.T) {
	c, rep := fiveInTwoSets(t)
	p := llm.Price{In: 2, Out: 10}
	var log bytes.Buffer
	c.Price, c.Log = &p, &log
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	rep.Results[4].Error = "evaluate: simulated" // L5 drops out: the second set is now {L3, L4}
	decideAll(rep, c.Policy, c.Seq)
	if len(rep.Sets) != 2 || rep.Sets[1].Of != 2 {
		t.Fatalf("setup: sets %+v", rep.Sets)
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(fb.submitted[1]) != 1 || !strings.HasPrefix(fb.submitted[1][0].CustomID, "S2-") {
		t.Fatalf("only the changed set is sent again: %d batches", len(fb.submitted))
	}
	if rep.Sets[0].By != "model" || rep.Sets[1].By != "model" || len(rep.Sets[1].Order) != 2 {
		t.Fatalf("sets %+v", rep.Sets)
	}
	three := llm.Usage{InputTokens: 3000, OutputTokens: 300} // S1, the old S2 (unused) and the new S2
	if math.Abs(rep.RankCostUSD-p.Cost(three, true)) > 1e-12 {
		t.Fatalf("rank cost %v, want %v: the collected answer no set used was paid for too", rep.RankCostUSD, p.Cost(three, true))
	}
	if !strings.Contains(log.String(), "1 rank answer") {
		t.Fatalf("the unused answer is logged:\n%s", log.String())
	}
	if exists(ex.statePath) {
		t.Fatal("state file left behind after ranking finished")
	}
}

// A set renumbered by a change earlier in the shoot sends the same frames under a
// new call ID: its recorded answer still applies (the model never sees the ID).
func TestRankBatchReusesAnswersWhenSetsRenumber(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	rep.Sets[0].ID = 4
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("%d batches, set %+v", len(fb.submitted), rep.Sets[0])
	}
}

// A submission cut off before its batch ID was recorded may have created the batch:
// a re-run refuses rather than pay twice.
func TestRankBatchRefusesInterruptedSubmission(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{failOn: 1, failErr: context.Canceled} // Ctrl-C mid-upload
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("expected the interrupted submit to fail")
	}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil || !strings.Contains(err.Error(), "not resubmitting") || len(fb.submitted) != 0 {
		t.Fatalf("err=%v submitted=%d", err, len(fb.submitted))
	}
}

// Any unknown-outcome submission in the file blocks sending more, even one for
// calls no current set makes: judge's batches refuse the same way.
func TestRankBatchRefusesAnySubmittingRecord(t *testing.T) {
	c, rep := seqShoot(t, 3)
	state := `{"version":1,"backend":"","model":"","batches":[{"round":0,"custom_ids":["S9-0000000000000000"],"status":"submitting"}]}`
	if err := os.WriteFile(c.ReportPath+".rank-batch.json", []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBatch{}
	if err := RankSets(context.Background(), rep, c, rankEx(fb, c), false); err == nil || !strings.Contains(err.Error(), "not resubmitting") || len(fb.submitted) != 0 {
		t.Fatalf("err=%v submitted=%d", err, len(fb.submitted))
	}
}

// A definitive rejection (a 4xx) fails only its chunk's sets, which fall back to
// scores; nothing is left pending, so no state blocks the next run.
func TestRankBatchRejectedChunkFallsBackToScores(t *testing.T) {
	c, rep := twoSets(t)
	c.BatchChunkBytes = 1 // one request per batch: S1's is rejected, S2's goes
	fb := &fakeBatch{failOn: 1, failErr: fmt.Errorf("%w: api status 400: too large", llm.ErrRejected)}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatalf("a rejected chunk is not a stop: %v", err)
	}
	if rep.Sets[0].By == "model" || rep.Sets[1].By != "model" || len(fb.submitted) != 1 || exists(ex.statePath) {
		t.Fatalf("sets %+v, %d batches, state kept %v", rep.Sets, len(fb.submitted), exists(ex.statePath))
	}
	b := &rankBackend{order: reverse}
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false); err != nil || b.calls != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("the next run ranks the rejected set: %v, %d calls", err, b.calls)
	}
}

// Answers read back from the state need no images: a frame that can't be read now
// (moved away, say) doesn't stop its set from being ranked by the recorded answer.
func TestRankBatchReplayNeedsNoImages(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	if err := os.Remove(rep.Results[1].File); err != nil {
		t.Fatal(err)
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("%d batches, set %+v", len(fb.submitted), rep.Sets[0])
	}
}

// Ctrl-C while a rank batch is processing keeps its record; the re-run re-attaches.
func TestRankBatchCtrlCWhilePollingKeepsState(t *testing.T) {
	c, rep := seqShoot(t, 3)
	c.BatchPoll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBatch{hold: true, onStatus: func(string) { cancel() }}
	ex := rankEx(fb, c)
	if err := RankSets(ctx, rep, c, ex, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if !exists(ex.statePath) || len(fb.submitted) != 1 || rep.Sets[0].By == "model" {
		t.Fatalf("state kept %v, %d batches", exists(ex.statePath), len(fb.submitted))
	}
	fb.hold, fb.onStatus = false, nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 || rep.Sets[0].By != "model" || exists(ex.statePath) {
		t.Fatalf("%d batches, set %+v", len(fb.submitted), rep.Sets[0])
	}
}

// A failure fetching an ended batch's results keeps its record; the re-run fetches
// them again.
func TestRankBatchResultsFailureKeepsState(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{resultsErr: errors.New("connection reset")}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("got %v", err)
	}
	if !exists(ex.statePath) || rep.Sets[0].By == "model" {
		t.Fatal("the batch record must survive a results failure")
	}
	fb.resultsErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("%d batches, set %+v", len(fb.submitted), rep.Sets[0])
	}
}

// --max-cost with batches: reached before the wave, nothing is sent and a recorded
// batch survives for a later run; reached by the wave, the ranking is finished and
// its state goes once the report is saved.
func TestRankBatchBudget(t *testing.T) {
	c, rep := seqShoot(t, 3)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := rankEx(fb, c)
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	fb.statusErr = nil
	c.Rank, c.rankWith, c.MaxCost = true, ex, 0.5
	if _, err := finishRun(context.Background(), rep, c, &spend{total: 1}); !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("over before the wave: %v", err)
	}
	if !exists(ex.statePath) || len(fb.submitted) != 1 || rep.Sets[0].By == "model" {
		t.Fatalf("the recorded batch must survive: state %v, %d batches", exists(ex.statePath), len(fb.submitted))
	}
	c.MaxCost = 1e-9
	if _, err := finishRun(context.Background(), rep, c, nil); !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("over after the wave: %v", err)
	}
	if exists(ex.statePath) || len(fb.submitted) != 1 || rep.Sets[0].By != "model" {
		t.Fatalf("state %v, %d batches, set %+v", exists(ex.statePath), len(fb.submitted), rep.Sets[0])
	}
}

// The rank state goes only once the report holding the ranking is saved: if that
// save fails, the re-run still finds every paid answer.
func TestFinishRunKeepsRankStateUntilReportSaved(t *testing.T) {
	c, rep := seqShoot(t, 3)
	fb := &fakeBatch{}
	c.Rank, c.rankWith = true, rankEx(fb, c)
	if err := os.Mkdir(c.ReportPath+".tmp", 0o755); err != nil { // the report's save fails
		t.Fatal(err)
	}
	if _, err := finishRun(context.Background(), rep, c, nil); err == nil {
		t.Fatal("the report save must fail")
	}
	if !exists(c.ReportPath + ".rank-batch.json") {
		t.Fatal("rank state dropped before the report holding the ranking was saved")
	}
	if err := os.Remove(c.ReportPath + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := finishRun(context.Background(), rep, c, nil); err != nil {
		t.Fatal(err)
	}
	if exists(c.ReportPath+".rank-batch.json") || len(fb.submitted) != 1 {
		t.Fatalf("state %v, %d batches", exists(c.ReportPath+".rank-batch.json"), len(fb.submitted))
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
	if len(fb.submitted) != 2 || len(fb.submitted[1]) != 1 || !strings.HasPrefix(fb.submitted[1][0].CustomID, "S1-") {
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
// --resume, and then re-attaches to the ranking batch, sending nothing again and
// charging the ranking once.
func TestBatchJudgeRankReattachesOnResume(t *testing.T) {
	c, p := rankShoot(t)
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
	if want := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, true); math.Abs(rep.RankCostUSD-want) > 1e-12 {
		t.Fatalf("rank cost %v, want %v (charged once)", rep.RankCostUSD, want)
	}
	if exists(c.ReportPath + ".rank-batch.json") {
		t.Fatal("rank state left behind")
	}
}
