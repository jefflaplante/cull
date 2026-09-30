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

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
)

func rankEx(fb *fakeBatch, c Config) batchExec {
	return batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json", rerun: rerunRankBatch}
}

func TestRankBatchRoundsAndCost(t *testing.T) {
	c, rep := seqShoot(t, 9)
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.Batch = &p, true
	fb := &fakeBatch{}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json", rerun: rerunRankBatch}
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
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json", rerun: rerunRankBatch}
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
	state := `{"version":2,"backend":"","model":"","batches":[{"round":0,"custom_ids":["S9-0000000000000000"],"status":"submitting"}]}`
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

// cancelAfterSubmit cancels the run right after its first batch is created (Ctrl-C
// between two chunk uploads).
type cancelAfterSubmit struct {
	*fakeBatch
	cancel func()
	n      int
}

func (c *cancelAfterSubmit) SubmitBatch(ctx context.Context, reqs []llm.BatchRequest) (string, error) {
	id, err := c.fakeBatch.SubmitBatch(ctx, reqs)
	c.n++
	if c.n == 1 && c.cancel != nil {
		c.cancel()
	}
	return id, err
}

// Re-review repro (a): Ctrl-C between the two chunk batches of a 9-frame set, then
// a frame of chunk 2 becomes unreadable. On the re-run chunk 1's paid answer is
// handed over, but its set fails before taking it: the ranking's cost must still
// hold it, exactly once.
func TestRankBatchChargesAnswerOfASetThatFailed(t *testing.T) {
	c, rep := seqShoot(t, 9)
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.BatchChunkBytes = &p, 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBatch{}
	cl := &cancelAfterSubmit{fakeBatch: fb, cancel: cancel}
	ex := batchExec{client: cl, cfg: c, statePath: c.ReportPath + ".rank-batch.json", rerun: rerunRankBatch}
	if err := RankSets(ctx, rep, c, ex, false); !errors.Is(err, context.Canceled) || len(fb.submitted) != 1 {
		t.Fatalf("run 1: %v, %d batches", err, len(fb.submitted))
	}
	if err := os.Remove(rep.Sets[0].Members[6]); err != nil { // L7, in chunk 2
		t.Fatal(err)
	}
	cl.cancel = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	one := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, true)
	if rep.Sets[0].By == "model" || math.Abs(rep.RankCostUSD-one) > 1e-12 || exists(ex.statePath) {
		t.Fatalf("set by %s, RankCostUSD %v, want %v (chunk 1's answer, once), state kept %v", rep.Sets[0].By, rep.RankCostUSD, one, exists(ex.statePath))
	}
}

// failFirstSave runs finishRun once with the report's save failing, then returns
// the report as the re-run finds it on disk (without that run's ranking).
func failFirstSave(t *testing.T, c Config, rep *report.Report) *report.Report {
	t.Helper()
	if err := os.Mkdir(c.ReportPath+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := finishRun(context.Background(), rep, c, nil); err == nil {
		t.Fatal("the report save must fail")
	}
	if err := os.Remove(c.ReportPath + ".tmp"); err != nil {
		t.Fatal(err)
	}
	saved, err := report.Load(c.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// Re-review repro (b): a run hands both sets' answers over but its report save
// fails; before the re-run set 2 changes, so its old answer is no longer asked for.
// No saved report holds that answer, so the re-run must charge it.
func TestRankBatchChargesUnsavedAnswerWhenItsSetChanges(t *testing.T) {
	c, rep := fiveInTwoSets(t)
	p := llm.Price{In: 2, Out: 10}
	fb := &fakeBatch{}
	c.Price = &p
	c.Rank, c.rankWith = true, rankEx(fb, c)
	rep = failFirstSave(t, c, rep)
	for i := 2; i < 5; i++ {
		rep.Results[i].Look = brightLook()
	}
	rep.Results[4].Error = "evaluate: simulated" // set 2 becomes {L3, L4}
	if _, err := finishRun(context.Background(), rep, c, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := report.Load(c.ReportPath)
	three := p.Cost(llm.Usage{InputTokens: 3000, OutputTokens: 300}, true) // S1, the old S2 (paid, unused), the new S2
	if err != nil || len(fb.submitted) != 2 || math.Abs(saved.RankCostUSD-three) > 1e-12 {
		t.Fatalf("%v: %d batches, saved RankCostUSD %v, want %v", err, len(fb.submitted), saved.RankCostUSD, three)
	}
}

// Re-review repro (c): a run hands an answer over but its report save fails; the
// next run stops before the wave (budget) and saves a report without it. That
// commit must not mark the answer charged: the run after that pays for it.
func TestRankBatchStopBeforeWaveKeepsUnsavedAnswerUncharged(t *testing.T) {
	c, rep := seqShoot(t, 3)
	p := llm.Price{In: 2, Out: 10}
	fb := &fakeBatch{}
	c.Price = &p
	c.Rank, c.rankWith = true, rankEx(fb, c)
	rep = failFirstSave(t, c, rep)
	c.MaxCost = 0.5
	if _, err := finishRun(context.Background(), rep, c, &spend{total: 1}); !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("budget stop expected, got %v", err)
	}
	rep, err := report.Load(c.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	c.MaxCost = 0
	if _, err := finishRun(context.Background(), rep, c, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := report.Load(c.ReportPath)
	one := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, true)
	if err != nil || len(fb.submitted) != 1 || saved.Sets[0].By != "model" || math.Abs(saved.RankCostUSD-one) > 1e-12 {
		t.Fatalf("%v: %d batches, saved RankCostUSD %v, want %v", err, len(fb.submitted), saved.RankCostUSD, one)
	}
}

// RankBatch does what Rank does, ranking through the Message Batches API: the
// set is ranked, its cost is at half the sync price for the same usage, and its
// rank-batch state is gone once the ranking (and the report holding it) is done.
func TestRankBatchRanksThenSavesThenDropsState(t *testing.T) {
	c, _ := seqShoot(t, 2)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	fb := &fakeBatch{}
	rep, err := RankBatch(context.Background(), c, fb, false)
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Sets[0]
	if s.By != "model" || len(s.Order) != 2 {
		t.Fatalf("set %+v", s)
	}
	syncCost := p.Cost(s.Usage, false)
	if rep.RankCostUSD <= 0 || math.Abs(rep.RankCostUSD-syncCost/2) > 1e-12 {
		t.Fatalf("rank cost %v, want half the sync price %v", rep.RankCostUSD, syncCost)
	}
	if exists(rankBatchStatePath(c)) {
		t.Fatal("rank batch state left behind")
	}
	saved, err := report.Load(c.ReportPath)
	if err != nil || saved.Sets[0].By != "model" {
		t.Fatalf("saved: %v %+v", err, saved)
	}
}

// The invariant this task exists to protect: RankBatch (via the shared rank())
// must not commit before the report holding the ranking is saved. When the
// save fails, the state's answers stay uncharged, so a following successful
// run charges them exactly once — never zero times (an unsaved ranking) and
// never twice (a charged-but-unsaved answer paid for again).
func TestRankBatchKeepsStateUnchargedWhenReportSaveFails(t *testing.T) {
	c, _ := seqShoot(t, 2)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	fb := &fakeBatch{}
	if err := os.Mkdir(c.ReportPath+".tmp", 0o755); err != nil { // blocks report.Save's atomic rename
		t.Fatal(err)
	}
	if _, err := RankBatch(context.Background(), c, fb, false); err == nil {
		t.Fatal("the report save must fail")
	}
	if !exists(rankBatchStatePath(c)) {
		t.Fatal("rank batch state must survive a failed save")
	}
	st, err := loadRankBatchState(rankBatchStatePath(c))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Answers) == 0 {
		t.Fatal("setup: the batch's answer must already be collected")
	}
	for k, a := range st.Answers {
		if a.Charged {
			t.Fatalf("answer %s must not be charged: its report was never saved", k)
		}
	}
	if err := os.Remove(c.ReportPath + ".tmp"); err != nil {
		t.Fatal(err)
	}
	rep, err := RankBatch(context.Background(), c, fb, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 {
		t.Fatalf("the re-run must reuse the recorded answer, not resubmit: %d batches", len(fb.submitted))
	}
	one := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, true)
	if rep.RankCostUSD != one {
		t.Fatalf("charged exactly once: rank cost %v, want %v", rep.RankCostUSD, one)
	}
	if exists(rankBatchStatePath(c)) {
		t.Fatal("rank batch state left behind")
	}
}

// Important #1 (review round 1): cli/rank.go now sets cfg.Backend/cfg.Model to
// what rank actually uses (applyBackendModel), matching judge's own
// cfg.Backend/cfg.Model. Without that, RankBatch's cfg carried "" / "", so a
// rank-batch state judge --batch's ranking round left behind (backend/model
// recorded) could never be re-attached by 'cull rank --batch' (backend/model
// "" / ""): batchExec.open's mismatch check refused it outright.
//
// This test exercises the round trip with the cfg values the fixed CLI now
// builds (both entry points share the same *rankShoot* cfg, backend
// "anthropic", model "claude-sonnet-5"): a rank-batch state judge --batch's
// ranking round left pending is picked up by a later RankBatch call (what
// 'cull rank --batch' runs) with no mismatch error.
func TestRankBatchReattachesToJudgeBatchsPendingRankingState(t *testing.T) {
	c, _ := rankShoot(t)                                                       // c.Backend, c.Model = "anthropic", "claude-sonnet-5"
	fb := &fakeBatch{statusErr: errors.New("network down"), statusErrID: "b2"} // the ranking batch
	if _, _, err := RunBatch(context.Background(), c, fb); err == nil {
		t.Fatal("the ranking's status failure must surface")
	}
	if !exists(rankBatchStatePath(c)) {
		t.Fatal("rank state must be recorded")
	}
	fb.statusErr = nil
	// 'cull rank --batch': the fixed CLI passes the same cfg.Backend/Model as
	// the judge run above (both come from the same judged-anthropic report).
	rep, err := RankBatch(context.Background(), c, fb, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sets[0].By != "model" {
		t.Fatalf("not ranked: %+v", rep.Sets[0])
	}
	if len(fb.submitted) != 2 {
		t.Fatalf("nothing should be resubmitted, only re-collected: %d batches", len(fb.submitted))
	}
}

// The reverse of the above: a rank-batch state 'cull rank --batch' (RankBatch)
// leaves pending must not break judge --batch --resume's own ranking round —
// which it would if RankBatch's cfg (as the CLI used to build it) recorded ""
// / "" while RunBatch's ranking round uses the real backend/model.
func TestJudgeBatchResumeReattachesToRankBatchsPendingState(t *testing.T) {
	c, _ := rankShoot(t)
	c.Rank = false // as if judged with --no-rank: the sets exist, unranked
	fb1 := &fakeBatch{}
	if _, _, err := RunBatch(context.Background(), c, fb1); err != nil {
		t.Fatal(err)
	}
	if exists(rankBatchStatePath(c)) {
		t.Fatal("setup: no ranking attempted yet")
	}
	// 'cull rank --batch', interrupted (its cfg.Backend/Model, as the fixed CLI
	// sets them, match this judged-anthropic report):
	fb2 := &fakeBatch{statusErr: errors.New("network down")}
	if _, err := RankBatch(context.Background(), c, fb2, false); err == nil {
		t.Fatal("expected the ranking's status failure to surface")
	}
	if !exists(rankBatchStatePath(c)) {
		t.Fatal("rank state must be recorded")
	}
	// judge --batch --resume, ranking on: must re-attach, not refuse on a
	// backend/model mismatch.
	c.Rank, c.Resume = true, true
	fb2.statusErr = nil
	rep, _, err := RunBatch(context.Background(), c, fb2)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sets[0].By != "model" {
		t.Fatalf("not ranked: %+v", rep.Sets[0])
	}
	if exists(rankBatchStatePath(c)) {
		t.Fatal("rank state left behind")
	}
}

// The model-mismatch guard still fires for rank: 'cull rank --batch --model
// claude-opus-5' after an interrupted run with a different model must refuse,
// not silently reuse the other model's answers and price them at this one's
// rate (Important #1's consequence (c)).
func TestRankBatchRefusesModelMismatch(t *testing.T) {
	c, _ := seqShoot(t, 3)
	c.Backend, c.Model = "anthropic", "claude-sonnet-5"
	fb := &fakeBatch{statusErr: errors.New("network down")}
	if _, err := RankBatch(context.Background(), c, fb, false); err == nil {
		t.Fatal("expected the ranking's status failure to surface")
	}
	fb.statusErr = nil
	c.Model = "claude-opus-5"
	_, err := RankBatch(context.Background(), c, fb, false)
	if err == nil || !strings.Contains(err.Error(), "belongs to anthropic/claude-sonnet-5") {
		t.Fatalf("got %v", err)
	}
}

// While RankBatch is stuck polling a batch that never ends, Ctrl-C's error names
// 'cull rank --batch' (this entry point's own re-run) and how to abandon the
// recorded ranking; its state survives to re-attach.
func TestRankBatchPendingKeepsStateWithRankHint(t *testing.T) {
	c, _ := seqShoot(t, 3)
	c.BatchPoll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBatch{hold: true, onStatus: func(string) { cancel() }}
	_, err := RankBatch(ctx, c, fb, false)
	if err == nil || !strings.Contains(err.Error(), "rerun cull rank with --batch") ||
		!strings.Contains(err.Error(), "delete "+rankBatchStatePath(c)+" to abandon it (what it already cost is paid; its answers are lost)") {
		t.Fatalf("got %v", err)
	}
	if !exists(rankBatchStatePath(c)) {
		t.Fatal("rank batch state must survive")
	}
}

// The same stuck-polling case reached through judge --batch's ranking round
// (RunBatch): the hint says to rerun with --batch --resume, judge's own re-run.
func TestJudgeBatchPendingHintSaysResume(t *testing.T) {
	c, _ := rankShoot(t)
	c.BatchPoll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBatch{holdID: "b2", onStatus: func(id string) {
		if id == "b2" {
			cancel()
		}
	}}
	_, _, err := RunBatch(ctx, c, fb)
	if err == nil || !strings.Contains(err.Error(), "--batch --resume") {
		t.Fatalf("got %v", err)
	}
	if !exists(rankBatchStatePath(c)) {
		t.Fatal("rank batch state must survive")
	}
}

// A Ctrl-C right as a batch chunk is about to be submitted (nothing sent yet)
// gives the same shape of re-attach hint collect() gives once a batch is
// submitted — but submitChunk is shared by judge's submit and rank's send, so
// it must use whatever hint its caller hands it, never a hard-coded one (that
// was Important #2: judge's own "--resume" hint was leaking into cull rank
// --batch). An arbitrary hint string, not either real caller's text, proves
// submitChunk doesn't hard-code either.
func TestSubmitChunkCtrlCKeepsCallersOwnHint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var recs []*batchRecord
	err := submitChunk(ctx, Config{}, &fakeBatch{}, &recs, []llm.BatchRequest{{CustomID: "x"}}, "judge", 1, "rerun with --waffles", func() error { return nil })
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "state saved; rerun with --waffles") {
		t.Fatalf("got %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("nothing sent: no record expected, got %+v", recs)
	}
}

// judge's own submit gives its own "--batch --resume" hint on a pre-submit
// Ctrl-C, reached through RunBatch (not a direct submitChunk call, so this
// guards the real wiring judge uses).
func TestJudgeSubmitCtrlCGivesResumeHint(t *testing.T) {
	_, c := batchShoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fb := &fakeBatch{}
	_, _, err := RunBatch(ctx, c, fb)
	if err == nil || !strings.Contains(err.Error(), "state saved; rerun with --batch --resume to re-attach") {
		t.Fatalf("got %v", err)
	}
	if len(fb.submitted) != 0 {
		t.Fatalf("nothing should be submitted: %d", len(fb.submitted))
	}
}

// A Ctrl-C right as cull rank --batch is about to submit a chunk (frames
// decoded, nothing sent yet) must give only cull rank's own hint, never
// judge's "--resume" (Important #2): the two are reached through the same
// shared submitChunk, so this exercises rank's own call site (send).
func TestRankBatchSendCtrlCGivesOnlyRankHintNeverResume(t *testing.T) {
	c, rep := seqShoot(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	fb := &fakeBatch{}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json", rerun: rerunRankBatch}
	st := &rankBatchState{Version: rankBatchStateVersion, Answers: map[string]*rankAnswer{}}
	j := newSetRank(&rep.Sets[0], rep.Sets[0].Members)
	calls := []rankCall{j.call("S1", []int{0, 1})}
	// A load that succeeds (frames "decoded") but cancels ctx right after, as if
	// Ctrl-C landed between decoding and the submit call.
	load := func(_ context.Context, calls []rankCall) error {
		cancel()
		for i := range calls {
			calls[i].Frames = make([]eval.RankFrame, len(calls[i].pos))
		}
		return nil
	}
	out := make([]rankOut, len(calls))
	err := ex.send(ctx, c, st, calls, []int{0}, load, func() error { return nil }, out)
	if err == nil || !strings.Contains(err.Error(), "rerun cull rank with --batch") || strings.Contains(err.Error(), "--resume") {
		t.Fatalf("got %v", err)
	}
	if len(fb.submitted) != 0 {
		t.Fatalf("nothing should be submitted: %d", len(fb.submitted))
	}
}

// With ranking on, a sync judge whose rank state is still pending a batch
// refuses before judging any frame: it would pay to judge again just to pay
// again to rank the same sets.
func TestSyncJudgeRefusesBeforeJudgingWhileRankBatchRecorded(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Rank = true
	p := rankBatchStatePath(c)
	if err := os.WriteFile(p, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{status: "sharp"}
	_, _, err := Run(context.Background(), c, b)
	if err == nil || !strings.Contains(err.Error(), p) || b.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, b.calls)
	}
}

// judge --no-rank (cfg.Rank == false) is unaffected by the guard above: a
// pending rank-batch state has nothing to do with a run that won't rank.
func TestNoRankJudgeIsUnaffectedByRankBatchGuard(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.Rank = false
	if err := os.WriteFile(rankBatchStatePath(c), []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{status: "sharp"}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatalf("--no-rank must ignore a pending rank-batch state: %v", err)
	}
	if b.calls == 0 {
		t.Fatal("judging must still happen")
	}
}

// Sync cull rank (pipeline.Rank) refuses before fillLooks too, while a rank
// batch is recorded: a v3 report's looks are not computed (a free but slow DNG
// decode pass) for a run about to refuse anyway.
func TestRankRefusesBeforeFillLooksWhileRankBatchRecorded(t *testing.T) {
	c, rep := seqShoot(t, 2)
	rep.SchemaVersion, rep.Sets = 3, nil
	for i := range rep.Results {
		rep.Results[i].Look, rep.Results[i].Group = "", nil
	}
	if err := rep.Save(c.ReportPath); err != nil {
		t.Fatal(err)
	}
	p := rankBatchStatePath(c)
	if err := os.WriteFile(p, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &rankBackend{order: reverse}
	if _, err := Rank(context.Background(), c, b, false); err == nil || !strings.Contains(err.Error(), p) || b.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, b.calls)
	}
	saved, err := report.Load(c.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range saved.Results {
		if r.Look != "" {
			t.Fatalf("looks must not be computed before the refusal: %+v", r)
		}
	}
}

// A fresh batch ranking goes out as one batch that can't be stopped midway, so
// an estimate over --max-cost refuses before anything is submitted. (A re-attach
// is exempt: TestRankBatchBudget's second half collects past a tiny budget.)
func TestRankBatchRefusesEstimateOverBudgetBeforeSubmitting(t *testing.T) {
	c, rep := seqShoot(t, 3)
	c.Price = &llm.Price{In: 10_000} // one estimated call (10k in) = $50 at batch price
	c.MaxCost = 1
	fb := &fakeBatch{}
	ex := rankEx(fb, c)
	err := RankSets(context.Background(), rep, c, ex, false)
	if !errors.Is(err, llm.ErrBudget) || !strings.Contains(err.Error(), "estimated") {
		t.Fatalf("want an up-front ErrBudget, got %v", err)
	}
	if len(fb.submitted) != 0 || exists(ex.statePath) {
		t.Fatalf("submitted %d batches, state %v", len(fb.submitted), exists(ex.statePath))
	}
}
