package pipeline

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// BatchClient is the Message Batches API (implemented by *llm.Anthropic).
type BatchClient interface {
	SubmitBatch(ctx context.Context, reqs []llm.BatchRequest) (string, error)
	BatchStatus(ctx context.Context, id string) (llm.BatchStatus, error)
	BatchResults(ctx context.Context, resultsURL string, schemaFor func(string) map[string]any, fn func(llm.BatchResult)) error
}

const (
	defaultChunkBytes = 32 << 20 // the API allows 256 MB; smaller uploads finish well inside the HTTP timeout
	maxBatchRequests  = 10_000
	batchStateVersion = 1
)

// batchState is everything needed to re-attach after a crash or Ctrl-C. It is
// saved before every submission and after every collection, so a batch that was
// paid for is never submitted twice.
type batchState struct {
	Version int                    `json:"version"`
	Backend string                 `json:"backend"`
	Model   string                 `json:"model"`
	Frames  map[string]*batchFrame `json:"frames"`
	Batches []*batchRecord         `json:"batches"`
}

type batchFrame struct {
	Result      report.Result      `json:"result"`
	Orientation int                `json:"orientation"`
	Stage       string             `json:"stage"` // locate, relocate, eval, done, error
	Locate      *eval.LocateResult `json:"locate,omitempty"`
	LocateErr   string             `json:"locate_err,omitempty"`
}

type batchRecord struct {
	ID        string   `json:"id,omitempty"`
	Round     int      `json:"round"`
	CustomIDs []string `json:"custom_ids"`
	Status    string   `json:"status"` // submitting, submitted, collected
}

func batchStatePath(cfg Config) string { return cfg.ReportPath + ".batch.json" }

func frameID(path string) string {
	h := sha1.Sum([]byte(path))
	return "f" + hex.EncodeToString(h[:8])
}

// RunBatch evaluates the shoot through the Message Batches API: half price,
// asynchronous. Round 1 sends evaluations for frames with a face and locate
// requests for the rest; round 2 evaluates the located frames. Polling can be
// interrupted: rerun with Resume to re-attach to batches already paid for.
func RunBatch(ctx context.Context, cfg Config, client BatchClient) (*report.Report, llm.Usage, error) {
	var total llm.Usage
	statePath := batchStatePath(cfg)
	if fileExists(rankBatchStatePath(cfg)) && !cfg.Resume {
		// Without --resume every frame would be judged (and paid for) again.
		return nil, total, fmt.Errorf("an unfinished batch ranking is recorded in %s: rerun with --batch --resume to re-attach (or delete it to start over)", rankBatchStatePath(cfg))
	}
	st, err := loadBatchState(statePath)
	switch {
	case err == nil && !cfg.Resume:
		return nil, total, fmt.Errorf("an unfinished batch run is recorded in %s: rerun with --batch --resume to re-attach (or delete it to start over)", statePath)
	case err == nil && (st.Backend != cfg.Backend || st.Model != cfg.Model):
		return nil, total, fmt.Errorf("%s belongs to %s/%s, not %s/%s", statePath, st.Backend, st.Model, cfg.Backend, cfg.Model)
	case errors.Is(err, fs.ErrNotExist):
		st = &batchState{Version: batchStateVersion, Backend: cfg.Backend, Model: cfg.Model, Frames: map[string]*batchFrame{}}
	case err != nil:
		return nil, total, err
	}
	for _, b := range st.Batches {
		if b.Status == "submitting" {
			return nil, total, fmt.Errorf("a batch submission was interrupted before its ID was recorded, so it may have been created: "+
				"check the Batches page in the Claude Console, then delete %s to start over (not resubmitting, to avoid paying twice)", statePath)
		}
	}
	rep, todo, err := startRun(&cfg)
	if err != nil {
		return nil, total, err
	}
	save := func() error { return saveState(statePath, st) }
	releaseUnsent(st)

	// Re-attach to anything already submitted.
	if err := collect(ctx, cfg, client, st, save); err != nil {
		return rep, total, err
	}

	// Round 1: frames not yet sent.
	var fresh []string
	for _, f := range todo {
		if _, ok := st.Frames[frameID(f)]; !ok {
			fresh = append(fresh, f)
		}
	}
	if len(fresh) > 0 && cfg.MaxCost > 0 && cfg.Price != nil {
		// A batch can't be stopped midway without losing work, so check up front.
		if usd, _, _ := llm.Estimate(len(fresh), *cfg.Price, true); usd > cfg.MaxCost {
			return rep, total, fmt.Errorf("%w: estimated $%.2f for %d frames exceeds --max-cost $%.2f", llm.ErrBudget, usd, len(fresh), cfg.MaxCost)
		}
	}
	reqs := prepareRound(cfg, fresh, st, func(p *prepared) (llm.BatchRequest, string, bool) {
		target, needLocate := faceTarget(cfg, p.frame, p.res.FocusTarget)
		if needLocate {
			small, err := p.frame.Downscaled(locateEdge, 85)
			if err != nil {
				p.res.Error = "locate: " + err.Error()
				return llm.BatchRequest{}, "error", false
			}
			return llm.BatchRequest{CustomID: "L-" + frameID(p.res.File), Req: eval.LocateRequest(small, locateMaxTokens)}, "locate", true
		}
		return evalRequest(cfg, p, target)
	})
	if err := submit(ctx, cfg, client, &st.Batches, reqs, "judge", 1, save); err != nil {
		return rep, total, err
	}
	if err := collect(ctx, cfg, client, st, save); err != nil {
		return rep, total, err
	}

	// Round 2: evaluate located frames with their subject crop.
	var relocate []string
	for _, f := range st.Frames {
		if f.Stage == "relocate" {
			relocate = append(relocate, f.Result.File)
		}
	}
	sort.Strings(relocate)
	// Workers write st.Frames under a lock; they read round 1 from this snapshot.
	prevs := make(map[string]batchFrame, len(relocate))
	for _, f := range relocate {
		prevs[frameID(f)] = *st.Frames[frameID(f)]
	}
	reqs = prepareRound(cfg, relocate, st, func(p *prepared) (llm.BatchRequest, string, bool) {
		prev := prevs[frameID(p.res.File)]
		p.res.Usage, p.res.FocusTarget = prev.Result.Usage, prev.Result.FocusTarget
		var lerr error
		if prev.LocateErr != "" {
			lerr = errors.New(prev.LocateErr)
		}
		return evalRequest(cfg, p, applyLocate(p.res.FocusTarget, prev.Locate, lerr, p.frame))
	})
	for id, prev := range prevs { // keep the locate answer in case round 2 must be resent
		if f := st.Frames[id]; f != nil {
			f.Locate, f.LocateErr = prev.Locate, prev.LocateErr
		}
	}
	if err := submit(ctx, cfg, client, &st.Batches, reqs, "judge", 2, save); err != nil {
		return rep, total, err
	}
	if err := collect(ctx, cfg, client, st, save); err != nil {
		return rep, total, err
	}

	// Finish: every sent frame is done or failed.
	ids := make([]string, 0, len(st.Frames))
	for id := range st.Frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	budget := &spend{} // the run's --max-cost covers the ranking too
	for _, id := range ids {
		f := st.Frames[id]
		if f.Stage != "done" && f.Stage != "error" {
			f.Result.Error = "batch: incomplete (stage " + f.Stage + ")"
		}
		if cfg.Price != nil {
			f.Result.CostUSD = cfg.Price.Cost(f.Result.Usage, true)
		}
		budget.add(f.Result.CostUSD, 0)
		total.Add(f.Result.Usage)
		rep.Results = append(rep.Results, f.Result)
		fmt.Fprintf(cfg.Log, "%s\n", summarize(f.Result))
	}
	// The report now holds every judged frame: save it, then drop the judge state.
	// The ranking below can stop (Ctrl-C while polling) after that, and a resume
	// that found both would add the frames twice.
	rep.Generated = time.Now()
	if err := rep.Save(cfg.ReportPath); err != nil {
		return rep, total, err
	}
	os.Remove(statePath)
	if cfg.Rank && !cfg.DryRun {
		cfg.rankWith = batchExec{client: client, cfg: cfg, statePath: rankBatchStatePath(cfg)}
	}
	used, err := finishRun(ctx, rep, cfg, budget)
	total.Add(used)
	return rep, total, err
}

// evalRequest builds a frame's evaluation request.
func evalRequest(cfg Config, p *prepared, target *focus.Target) (llm.BatchRequest, string, bool) {
	in, err := buildInput(cfg, p, target)
	if err != nil {
		p.res.Error = err.Error()
		return llm.BatchRequest{}, "error", false
	}
	return llm.BatchRequest{CustomID: "E-" + frameID(p.res.File), Req: eval.EvalRequest(in)}, "eval", true
}

// prepareRound prepares frames in parallel, records each in the state, and
// returns the requests to send.
func prepareRound(cfg Config, files []string, st *batchState, build func(*prepared) (llm.BatchRequest, string, bool)) []llm.BatchRequest {
	var mu sync.Mutex
	var reqs []llm.BatchRequest
	jobs := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < max(1, cfg.Concurrency); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				p, err := prepareFrame(cfg, path)
				stage := "error"
				var req llm.BatchRequest
				ok := false
				if err == nil {
					req, stage, ok = build(p)
				}
				mu.Lock()
				st.Frames[frameID(path)] = &batchFrame{Result: p.res, Orientation: p.orientation, Stage: stage}
				if ok {
					reqs = append(reqs, req)
				}
				mu.Unlock()
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].CustomID < reqs[j].CustomID })
	return reqs
}

// submit sends requests in size-capped chunks, recording each batch in recs (and
// saving) before and after its create call. what ("judge", "rank") is for the log.
func submit(ctx context.Context, cfg Config, client BatchClient, recs *[]*batchRecord, reqs []llm.BatchRequest, what string, round int, save func() error) error {
	limit := cfg.BatchChunkBytes
	if limit <= 0 {
		limit = defaultChunkBytes
	}
	for len(reqs) > 0 {
		if err := ctx.Err(); err != nil {
			return err // nothing sent for this chunk: no record, nothing to refuse later
		}
		n, size := 0, 0
		for n < len(reqs) && n < maxBatchRequests {
			size += requestSize(reqs[n])
			if n > 0 && size > limit {
				break
			}
			n++
		}
		chunk := reqs[:n]
		reqs = reqs[n:]
		rec := &batchRecord{Round: round, Status: "submitting"}
		for _, r := range chunk {
			rec.CustomIDs = append(rec.CustomIDs, r.CustomID)
		}
		*recs = append(*recs, rec)
		if err := save(); err != nil {
			return err
		}
		id, err := client.SubmitBatch(ctx, chunk)
		if errors.Is(err, llm.ErrRejected) {
			*recs = (*recs)[:len(*recs)-1] // definitively refused: nothing was created
			if serr := save(); serr != nil {
				return serr
			}
			return fmt.Errorf("submit batch: %w", err)
		}
		if err != nil {
			// Interrupted, timed out or a server error: the batch may exist. Keep the
			// "submitting" record so a resume refuses instead of paying twice.
			return fmt.Errorf("submit batch: outcome unknown (%w); check the Claude Console before resuming", err)
		}
		rec.ID, rec.Status = id, "submitted"
		if err := save(); err != nil {
			return err
		}
		fmt.Fprintf(cfg.Log, "submitted batch %s (%s round %d, %d requests)\n", id, what, round, len(chunk))
	}
	return nil
}

// requestSize approximates a request's JSON size: base64 images plus text.
func requestSize(r llm.BatchRequest) int {
	n := 2048 + len(r.Req.System)
	for _, p := range r.Req.Parts {
		n += len(p.Text) + len(p.JPEG)*4/3
	}
	return n
}

// collect polls every submitted batch until it ends and applies its results.
func collect(ctx context.Context, cfg Config, client BatchClient, st *batchState, save func() error) error {
	for _, b := range st.Batches {
		if b.Status != "submitted" {
			continue
		}
		status, err := await(ctx, cfg, client, b.ID)
		if err != nil {
			return fmt.Errorf("%w (state saved; rerun with --batch --resume to re-attach)", err)
		}
		schemaFor := func(id string) map[string]any {
			if strings.HasPrefix(id, "L-") {
				return eval.LocateSchema()
			}
			return eval.EvaluationSchema()
		}
		err = client.BatchResults(ctx, status.ResultsURL, schemaFor, func(r llm.BatchResult) { applyResult(cfg, st, r) })
		if err != nil {
			return fmt.Errorf("batch %s results: %w (rerun with --batch --resume)", b.ID, err)
		}
		b.Status = "collected"
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// await polls batch id every cfg.BatchPoll until it ends. Once ctx is cancelled it
// returns ctx's error.
func await(ctx context.Context, cfg Config, client BatchClient, id string) (llm.BatchStatus, error) {
	for {
		status, err := client.BatchStatus(ctx, id)
		if err != nil {
			return status, fmt.Errorf("batch %s: %w", id, err)
		}
		if status.Ended {
			return status, nil
		}
		fmt.Fprintf(cfg.Log, "batch %s: %s\n", id, counts(status.Counts))
		select {
		case <-time.After(cfg.BatchPoll):
		case <-ctx.Done():
			return status, ctx.Err()
		}
	}
}

func counts(c map[string]int) string {
	var parts []string
	for _, k := range []string{"processing", "succeeded", "errored", "canceled", "expired"} {
		if c[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, c[k]))
		}
	}
	if len(parts) == 0 {
		return "in progress"
	}
	return strings.Join(parts, ", ")
}

func applyResult(cfg Config, st *batchState, r llm.BatchResult) {
	if len(r.CustomID) < 3 {
		return
	}
	f := st.Frames[r.CustomID[2:]]
	if f == nil {
		return
	}
	if r.Response != nil {
		f.Result.Usage.Add(r.Response.Usage)
	}
	switch r.CustomID[:2] {
	case "L-":
		f.Stage = "relocate"
		if r.Err != nil {
			f.LocateErr = r.Err.Error()
			return
		}
		loc, err := eval.DecodeLocate(r.Response.JSON)
		if err != nil {
			f.LocateErr = err.Error()
			return
		}
		f.Locate = loc
	case "E-":
		if r.Err != nil {
			f.Result.Error, f.Stage = "evaluate: "+r.Err.Error(), "error"
			return
		}
		e, err := eval.DecodeEvaluation(r.Response.JSON)
		if err != nil {
			f.Result.Error, f.Stage = "evaluate: "+err.Error(), "error"
			return
		}
		finish(cfg, &f.Result, e, f.Orientation)
		f.Stage = "done"
	}
}

func loadBatchState(path string) (*batchState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st batchState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Frames == nil {
		st.Frames = map[string]*batchFrame{}
	}
	return &st, nil
}

// saveState writes a batch state atomically: a torn state file could lose a batch ID.
func saveState(path string, st any) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

// releaseUnsent returns frames that were prepared but never submitted (a later
// chunk failed, or the run stopped between chunks) to the rounds that send them:
// round-2 frames keep their locate answer, round-1 frames are prepared afresh.
func releaseUnsent(st *batchState) {
	sent := map[string]bool{}
	for _, b := range st.Batches {
		for _, id := range b.CustomIDs {
			sent[id] = true
		}
	}
	for fid, f := range st.Frames {
		var id string
		switch f.Stage {
		case "locate":
			id = "L-" + fid
		case "eval":
			id = "E-" + fid
		default:
			continue
		}
		if sent[id] {
			continue
		}
		if f.Locate != nil || f.LocateErr != "" {
			f.Stage = "relocate"
		} else {
			delete(st.Frames, fid)
		}
	}
}
