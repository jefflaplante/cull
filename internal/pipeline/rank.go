package pipeline

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"path/filepath"
	"sort"
	"sync"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
)

// The rank stage: once every frame is judged and grouped, each set of two or more
// rankable frames is compared side by side by the model (in chunks above
// maxRankFrames), and its order is stored in the report's Set for decideAll.

const (
	rankFullEdge      = 768  // long edge of a frame's full view in a rank call
	rankCropMax       = 512  // largest side of its subject crop, native resolution
	defaultRankTokens = 2048 // 8 frames' notes and a summary take a few hundred
)

// rankCall is one rank request, frames in capture order. Frames are empty until an
// executor's loader fills them: only calls actually sent need images.
type rankCall struct {
	ID     string   // "S<set>"; a chunked set sends "S<set>-C<k>", then "S<set>-F"
	Files  []string // the frames' Result.File, in the same order
	Frames []eval.RankFrame

	job *setRank // the set it ranks, and its frames' positions in it
	pos []int
}

// loader fills the Frames of calls, decoding each frame once. A call it leaves
// without Frames can't be sent: a frame of its set couldn't be read (the set has
// failed), or ctx was cancelled, whose error it then returns.
type loader func(ctx context.Context, calls []rankCall) error

// rankOut answers one rankCall. R can come with an llm.ErrQuotaStop Err: keep it.
type rankOut struct {
	ID  string
	R   *eval.Ranking
	U   eval.Usage
	Err error
	key string // the recorded batch answer it carries (framesKey); "" from a sync call
}

// rankRun is what one ranking run put in the report, for the executor's commit
// once that report is saved.
type rankRun struct {
	finished bool     // every set it took on recorded and settled, nothing left pending
	charged  []string // the batch answers (framesKeys) whose usage reached the report
}

// rankExec runs one round of rank calls and answers each, in any order (matched
// by ID), loading images (load) only for the calls it sends. syncExec calls a
// backend; batchExec (rank_batch.go) sends Message Batches and reuses answers it
// has recorded.
type rankExec interface {
	Run(ctx context.Context, calls []rankCall, load loader) []rankOut
	// batch: calls are billed at the batch rate, and every set goes in one wave
	// (one round of chunk calls, one of finals), since a batch can't be stopped
	// midway. Price and wave shape follow the executor, never cfg.Batch.
	batch() bool
	// recorded reports whether a batch ranking is already recorded for this
	// executor (a re-attach): what it collects was paid for when it was sent.
	recorded() bool
	// settle runs once every set is recorded. It returns the usage of anything paid
	// for that isn't in used (the answers this run took into the report) and was
	// never charged to a saved report: an answer for a set that changed, one handed
	// to a set that failed, or one from a run whose report was never saved. The
	// ranking's cost must still include it.
	settle(ctx context.Context, used map[string]bool) (llm.Usage, error)
	// commit runs once the report holding the run is saved: run.charged is now
	// charged in it, and when the ranking finished the executor's state goes.
	commit(run rankRun) error
}

// syncExec runs rank calls on a backend through a worker pool. After a quota stop
// or an abort it sends no more calls; those answer with the stop as their Err.
type syncExec struct {
	b           llm.Backend
	concurrency int
	maxTokens   int // 0 = defaultRankTokens
}

func (s syncExec) Run(ctx context.Context, calls []rankCall, load loader) []rankOut {
	tokens := s.maxTokens
	if tokens <= 0 {
		tokens = defaultRankTokens
	}
	calls = append([]rankCall(nil), calls...)
	load(ctx, calls) // a cancelled ctx answers every call "not sent" below
	out := make([]rankOut, len(calls))
	var mu sync.Mutex
	var stopped error
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < max(1, s.concurrency); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i].ID = calls[i].ID
				mu.Lock()
				stop := stopped
				mu.Unlock()
				if stop == nil {
					stop = ctx.Err()
				}
				if stop != nil {
					out[i].Err = fmt.Errorf("not sent: %w", stop)
					continue
				}
				if calls[i].Frames == nil {
					out[i].Err = errUnreadable
					continue
				}
				out[i].R, out[i].U, out[i].Err = eval.Rank(ctx, s.b, calls[i].Frames, tokens)
				if isStop(out[i].Err) {
					mu.Lock()
					if stopped == nil {
						stopped = out[i].Err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := range calls {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

func (syncExec) batch() bool    { return false }
func (syncExec) recorded() bool { return false }
func (syncExec) settle(context.Context, map[string]bool) (llm.Usage, error) {
	return llm.Usage{}, nil
}
func (syncExec) commit(rankRun) error { return nil }

// errUnreadable answers a call whose set has a frame that couldn't be read.
var errUnreadable = errors.New("not sent: a frame of its set couldn't be read")

// isStop: an error that ends the stage, not just one call.
func isStop(err error) bool {
	return errors.Is(err, llm.ErrQuotaStop) || errors.Is(err, llm.ErrAbortRun) || errors.Is(err, errBatchPending)
}

// rankPrice prices ranking usage at the executor's rate.
func rankPrice(cfg Config, u llm.Usage, batch bool) float64 {
	if cfg.Price == nil {
		return 0
	}
	return cfg.Price.Cost(u, batch)
}

// rankImages re-extracts a frame's preview from where it lives now (MovedTo once
// moved) and returns its rank-call images: the whole frame at rankFullEdge px, and
// judge's focus box at native resolution, capped at rankCropMax px around its
// centre (nil without a box).
func rankImages(r report.Result) (eval.RankFrame, error) {
	f, err := decodeWhereItLives(r)
	if err != nil {
		return eval.RankFrame{}, err
	}
	var rf eval.RankFrame
	if rf.Full, err = f.Downscaled(rankFullEdge, 82); err != nil {
		return eval.RankFrame{}, fmt.Errorf("encode: %w", err)
	}
	if ft := r.FocusTarget; ft != nil && ft.Box != nil && ft.Box.Valid() {
		t := focus.BoxTarget(denorm(*ft.Box, f.W, f.H))
		rect := capAround(focus.SubjectRect(t, f.W, f.H), t.Center, rankCropMax)
		if rf.Crop, err = f.Crop(rect, 90); err != nil {
			return eval.RankFrame{}, fmt.Errorf("crop: %w", err)
		}
	}
	return rf, nil
}

// capAround shrinks r to at most side×side, centred on c as far as r allows.
func capAround(r image.Rectangle, c image.Point, side int) image.Rectangle {
	w, h := min(side, r.Dx()), min(side, r.Dy())
	x0 := min(max(c.X-w/2, r.Min.X), r.Max.X-w)
	y0 := min(max(c.Y-h/2, r.Min.Y), r.Max.Y-h)
	return image.Rect(x0, y0, x0+w, y0+h)
}

// decodeWhereItLives decodes a frame's preview from its current location.
func decodeWhereItLives(r report.Result) (*imageprep.Frame, error) {
	path := r.File
	if r.MovedTo != "" {
		path = r.MovedTo
	}
	pv, err := dng.Best(path, 0)
	if err != nil {
		return nil, err
	}
	return imageprep.Decode(pv.Data, pv.Orientation)
}

// lookProgressEvery is how often fillLooks logs progress.
const lookProgressEvery = 50

// fillLooks computes the look of every measured frame that has none (a schema-v3
// report) from its preview, read from where the frame lives now. A frame that
// can't be read keeps none, so it joins no set. It returns how many it filled; once
// ctx is cancelled it stops between frames with ctx's error, leaving the rest
// without a look (which decideAll takes: they join no set).
//
// On real DNGs this is ~1s/frame (a full-preview decode), so a large schema-v3
// report can spend many silent minutes here: log progresses every
// lookProgressEvery frames to w, which may be nil (io.Discard).
func fillLooks(ctx context.Context, rep *report.Report, w io.Writer) (int, error) {
	if w == nil {
		w = io.Discard
	}
	var todo []int
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.Look == "" && r.Error == "" && r.Preview != nil {
			todo = append(todo, i)
		}
	}
	n := 0
	for _, i := range todo {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		r := &rep.Results[i]
		f, err := decodeWhereItLives(*r)
		if err != nil {
			continue
		}
		r.Look = report.EncodeLook(f.Grid(group.LookSize))
		n++
		if n%lookProgressEvery == 0 {
			fmt.Fprintf(w, "computing looks: %d/%d\n", n, len(todo))
		}
	}
	return n, nil
}

// Rank ranks an existing report's sets without judging again (cull rank). It loads
// the report, computes missing looks from the DNGs (a schema-v3 report), decides,
// ranks the sets that need it (every multi-frame set when force) on b, decides
// again, then writes sidecars and syncs culled/ as Decide does (cfg's WriteXMP,
// XMPDevelop, OverwriteXMP, MoveCulled, Labels), and saves the report as the
// current schema. A ranking stop (budget, quota, Ctrl-C) is returned after the
// report, with every ranking paid for so far, is saved.
//
// It refuses up front, before fillLooks, while an unfinished batch ranking is
// recorded for this report: ranking on a sync backend would pay for the same
// sets again. Rerun with --batch to re-attach to it, or delete it to abandon it.
func Rank(ctx context.Context, cfg Config, b llm.Backend, force bool) (*report.Report, error) {
	if err := RankBatchPending(cfg); err != nil {
		return nil, err
	}
	if p, ok := b.(llm.ModelPinner); ok {
		cfg.pinner = p
	}
	ex := syncExec{b: withEffort(b, cfg), concurrency: cfg.Concurrency, maxTokens: cfg.RankTokens}
	return rank(ctx, cfg, ex, force)
}

// RankBatch is Rank, ranking through the Message Batches API (half price,
// asynchronous) instead of a synchronous backend. Ctrl-C, a network failure
// while polling, or a batch left pending keeps its state in
// rankBatchStatePath(cfg): rerun 'cull rank --batch' to re-attach to it.
func RankBatch(ctx context.Context, cfg Config, client BatchClient, force bool) (*report.Report, error) {
	ex := batchExec{client: client, cfg: cfg, statePath: rankBatchStatePath(cfg), rerun: rerunRankBatch}
	return rank(ctx, cfg, ex, force)
}

// rank is Rank/RankBatch, calling on ex (sync or batch). Do not build this on
// the exported RankSets: RankSets commits before any save, which brings back
// the zero-charge bug (a paid answer must be charged exactly once per saved
// report). It keeps the ordering fillLooks -> (Ctrl-C: save looks, return) ->
// redecide (which ranks via rankSets) -> save -> ex.commit(run): commit only
// after the report holding the run is saved.
func rank(ctx context.Context, cfg Config, ex rankExec, force bool) (*report.Report, error) {
	log := cfg.Log
	if log == nil {
		log = io.Discard
	}
	rep, err := report.Load(cfg.ReportPath)
	if err != nil {
		return nil, err
	}
	rep.Relocate(cfg.ReportPath, cfg.Dir) // a renamed shoot folder: the save below keeps the new paths
	if cfg.pinner != nil && rep.ResolvedModel != "" {
		cfg.pinner.Pin(rep.ResolvedModel) // rank with the model the frames were judged with
	}
	n, err := fillLooks(ctx, rep, log)
	if n > 0 {
		fmt.Fprintf(log, "computed the look of %d frame(s) from their DNGs\n", n)
	}
	if err != nil { // Ctrl-C: keep the looks computed so far (free but slow); decide and rank nothing
		if n > 0 {
			err = errors.Join(err, rep.Save(cfg.ReportPath))
		}
		return rep, err
	}
	o := DecideOptions{Dir: cfg.Dir, Policy: cfg.Policy, WriteXMP: cfg.WriteXMP, XMPDevelop: cfg.XMPDevelop, OverwriteXMP: cfg.OverwriteXMP,
		MoveCulled: cfg.MoveCulled, Seq: cfg.Seq, Labels: cfg.Labels}
	var rankErr error
	var run rankRun
	if _, err := redecide(rep, o, log, func() { _, run, rankErr = rankSets(ctx, rep, cfg, ex, force, &spend{}) }); err != nil {
		return rep, err
	}
	if cfg.pinner != nil && rep.ResolvedModel == "" {
		rep.ResolvedModel = cfg.pinner.Resolved() // a report judged before this was recorded
	}
	if err := rep.Save(cfg.ReportPath); err != nil {
		return rep, err
	}
	return rep, errors.Join(rankErr, ex.commit(run)) // only now can the executor's state go
}

// RankSets asks the model to rank the sets that need it (every set of two or more
// rankable frames when force) and stores each answer in its Set: Order, Notes,
// Summary and By "model". The set's Usage and CostUSD grow by what its calls cost,
// and so does rep.RankCostUSD, whether or not the ranking succeeded. Run decideAll
// before (it builds the sets and marks rankable frames) and after (it applies the
// orders).
//
// Sets run in waves, cfg.Concurrency sets at a time (all at once with a batch
// executor: one round of chunk calls, then one of finals). cfg.MaxCost is checked
// before each wave; once it is reached no further set starts and the error wraps
// llm.ErrBudget. A set whose call fails is logged and stays by scores. A quota stop,
// an abort, a batch left pending or a cancelled context ends the stage once the
// wave in flight is recorded. While a batch ranking is recorded for the report, a
// sync executor refuses to rank: it would pay for the same sets again.
//
// RankSets saves no report, so it commits the executor's state at once (rep holds
// the ranking): callers that save one use rankSets, then commit after the save.
func RankSets(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, force bool) error {
	_, run, err := rankSets(ctx, rep, cfg, ex, force, &spend{})
	return errors.Join(err, ex.commit(run))
}

// rankSets is RankSets charging a run's budget, which may already hold the cost of
// the run's judge calls, and leaving the commit to the caller. It returns the
// ranking's usage and what the run put in rep (see rankRun).
func rankSets(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, force bool, budget *spend) (llm.Usage, rankRun, error) {
	var run rankRun
	var total llm.Usage
	log := cfg.Log
	if log == nil {
		log = io.Discard
	}
	todo := rankTodo(rep, force)
	if len(todo) > 0 && !ex.batch() {
		if err := RankBatchPending(cfg); err != nil {
			return total, run, err
		}
	}
	wave := max(1, cfg.Concurrency)
	if ex.batch() {
		wave = max(1, len(todo))
	}
	byFile := make(map[string]int, len(rep.Results))
	for i, r := range rep.Results {
		byFile[r.File] = i
	}
	if ex.batch() && !ex.recorded() && len(todo) > 0 && cfg.MaxCost > 0 && cfg.Price != nil {
		// One batch holds every set and can't be stopped midway without losing it,
		// so check up front, as RunBatch does for judging. A re-attach is exempt:
		// what it collects is already paid for.
		usd, _, _ := llm.EstimateRank(callsFor(rep, todo, cfg.RankTwice), *cfg.Price, true)
		if _, spent := budget.add(0, 0); spent+usd > cfg.MaxCost {
			return total, run, fmt.Errorf("%w: ranking estimated at $%.2f with $%.2f already spent exceeds --max-cost $%.2f; %d set(s) left by scores",
				llm.ErrBudget, usd, spent, cfg.MaxCost, len(todo))
		}
	}
	if len(todo) > 0 {
		cfg.stage(ui.Stage{Name: "rank", Unit: "sets", Total: int64(len(todo))})
		defer cfg.stage(ui.Stage{Name: "rank", Done: true})
	}
	var last error // the last wave's budget stop: the ranking still finished
	for len(todo) > 0 {
		if err := ctx.Err(); err != nil {
			return total, run, err
		}
		if over, spent := budget.add(0, cfg.MaxCost); over {
			return total, run, fmt.Errorf("%w: $%.2f of $%.2f; %d set(s) left by scores", llm.ErrBudget, spent, cfg.MaxCost, len(todo))
		}
		n := min(wave, len(todo))
		u, charged, err := rankWave(ctx, rep, cfg, ex, todo[:n], byFile, budget, log)
		total.Add(u)
		run.charged = append(run.charged, charged...)
		todo = todo[n:]
		cfg.stage(ui.Stage{Name: "rank", Add: int64(n)})
		switch {
		case err == nil:
		case errors.Is(err, llm.ErrBudget) && len(todo) > 0:
			return total, run, fmt.Errorf("%w; %d set(s) left by scores", err, len(todo))
		case errors.Is(err, llm.ErrBudget):
			last = err
		default:
			return total, run, err
		}
	}
	used := make(map[string]bool, len(run.charged))
	for _, k := range run.charged {
		used[k] = true
	}
	u, err := ex.settle(ctx, used)
	if c := rankPrice(cfg, u, ex.batch()); u.TotalIn()+u.OutputTokens > 0 {
		rep.RankCostUSD += c
		total.Add(u)
		budget.add(c, 0)
	}
	if err != nil {
		return total, run, errors.Join(last, err)
	}
	if last == nil {
		last = ctx.Err()
	}
	run.finished = true
	return total, run, last
}

// setRank is one set's ranking in progress. Positions index files and frames.
type setRank struct {
	set      *report.Set
	files    []string         // rankable members, capture order
	frames   []eval.RankFrame // decoded on demand (loadCallImages), kept for the next round
	decoded  []bool
	parts    [][]int // chunks of positions: one part = a single call
	orders   [][]int // each part's answer, positions best first
	final    []int   // positions sent to the final call, capture order
	order    []int   // the set's order, best first
	reversed []int   // with RankTwice: the order when the frames were shown reversed, best first
	notes    map[int]eval.RankEntry
	summary  string
	usage    llm.Usage
	keys     []string // batch answers whose usage is in usage
	err      error
}

func (j *setRank) fail(err error) {
	if j.err == nil {
		j.err = err
	}
}

// newSetRank starts a set's ranking over its rankable members.
func newSetRank(set *report.Set, files []string) *setRank {
	return &setRank{set: set, files: files, notes: map[int]eval.RankEntry{},
		frames: make([]eval.RankFrame, len(files)), decoded: make([]bool, len(files))}
}

// rankWave ranks a group of sets: round 1 (a single call per set, or its chunk
// calls), round 2 (the chunked sets' finals), then records each set. Frames are
// decoded only for the calls the executor sends. It also returns the batch answers
// (framesKeys) whose usage it put in rep; an answer a failed set never took is not
// among them, so settle charges it.
func rankWave(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, sets []int, byFile map[string]int, budget *spend, log io.Writer) (llm.Usage, []string, error) {
	var jobs []*setRank
	for _, si := range sets {
		var files []string
		for _, f := range rep.Sets[si].Members {
			if i, ok := byFile[f]; ok && rep.Results[i].Group != nil && rep.Results[i].Group.Rank > 0 {
				files = append(files, f)
			}
		}
		if len(files) < 2 {
			continue // nothing to compare (groups not decided)
		}
		j := newSetRank(&rep.Sets[si], files)
		j.parts = chunks(len(j.files))
		if len(j.parts) > 1 {
			if n := len(finalPositions(j.parts, j.parts, cfg.Policy.KeepBest)); n > maxRankFrames {
				j.fail(fmt.Errorf("its final round would compare %d frames, more than %d", n, maxRankFrames))
			}
		}
		jobs = append(jobs, j)
	}
	load := func(ctx context.Context, calls []rankCall) error { return loadCallImages(ctx, rep, cfg, calls, byFile) }

	// Round 1. With RankTwice a set that fits one call is also sent reversed: the
	// model favours some positions, and only places both orders agree on count.
	var calls []rankCall
	for _, j := range jobs {
		if j.err != nil {
			continue
		}
		for k, part := range j.parts {
			calls = append(calls, j.call(j.partID(k), part))
		}
		if cfg.RankTwice && len(j.parts) == 1 {
			calls = append(calls, j.call(j.reversedID(), reversedPositions(j.parts[0])))
		}
	}
	outs := answers(ctx, ex, calls, load)
	var stop error
	for _, j := range jobs {
		if j.err != nil {
			continue
		}
		if cfg.RankTwice && len(j.parts) == 1 {
			r, err := j.takeReversed(outs, j.reversedID(), reversedPositions(j.parts[0]))
			if isStop(err) && stop == nil {
				stop = err
			}
			j.reversed = r
		}
		j.orders = make([][]int, len(j.parts))
		for k, part := range j.parts {
			r, err := j.take(outs, j.partID(k), part)
			if isStop(err) && stop == nil {
				stop = err
			}
			if r == nil {
				continue
			}
			j.orders[k] = r
		}
		if j.err == nil && len(j.parts) == 1 {
			j.order = j.orders[0]
		}
	}

	// Round 2: finals of the chunked sets, unless round 1 hit a stop.
	calls = nil
	for _, j := range jobs {
		if j.err != nil || len(j.parts) == 1 {
			continue
		}
		if stop != nil {
			j.fail(fmt.Errorf("final round not sent: %w", stop))
			continue
		}
		j.final = finalPositions(j.parts, j.orders, cfg.Policy.KeepBest)
		calls = append(calls, j.call(fmt.Sprintf("S%d-F", j.set.ID), j.final))
	}
	if len(calls) > 0 {
		outs = answers(ctx, ex, calls, load)
		for _, j := range jobs {
			if j.err != nil || len(j.parts) == 1 {
				continue
			}
			r, err := j.take(outs, fmt.Sprintf("S%d-F", j.set.ID), j.final)
			if isStop(err) && stop == nil {
				stop = err
			}
			if r != nil {
				j.order = merge(j.orders, r)
			}
		}
	}
	// Record: every set pays for its calls; a complete answer ranks it. A set left
	// waiting on a pending batch is ranked by a re-run, which reuses each answer
	// recorded so far (batchExec) without paying for it again.
	var total llm.Usage
	var charged []string
	over, spent := false, 0.0
	for _, j := range jobs {
		s := j.set
		c := rankPrice(cfg, j.usage, ex.batch())
		charged = append(charged, j.keys...)
		s.Usage.Add(j.usage)
		s.CostUSD += c
		rep.RankCostUSD += c
		total.Add(j.usage)
		if o, t := budget.add(c, cfg.MaxCost); o {
			over, spent = true, t
		}
		if j.err != nil {
			if !errors.Is(j.err, errBatchPending) { // the stage's error says it once
				cfg.warn("set %d (%d frames) not ranked: %v", s.ID, len(j.files), j.err)
			}
			continue
		}
		s.Order, s.Notes = nil, nil
		for _, pos := range j.order {
			f := j.files[pos]
			s.Order = append(s.Order, f)
			if e, ok := j.notes[pos]; ok {
				s.Notes = append(s.Notes, report.RankNote{File: f, Strength: e.Strength, Weakness: e.Weakness})
			}
		}
		s.Reversed = nil
		for _, pos := range j.reversed {
			s.Reversed = append(s.Reversed, j.files[pos])
		}
		if cfg.RankTwice && len(j.parts) == 1 && j.reversed == nil {
			cfg.warn("set %d: the reversed ranking failed; ranked once", s.ID)
		}
		s.Summary, s.By = j.summary, "model"
		fmt.Fprintf(log, "ranked set %d (%d frames): %s wins — %s\n", s.ID, len(j.files), filepath.Base(s.Order[0]), s.Summary)
	}
	switch {
	case stop != nil:
		return total, charged, stop
	case over:
		return total, charged, fmt.Errorf("%w: $%.2f of $%.2f", llm.ErrBudget, spent, cfg.MaxCost)
	}
	return total, charged, nil
}

// partID is the call ID of round-1 part k: "S<set>" when the set fits one call.
func (j *setRank) partID(k int) string {
	if len(j.parts) == 1 {
		return fmt.Sprintf("S%d", j.set.ID)
	}
	return fmt.Sprintf("S%d-C%d", j.set.ID, k+1)
}

// reversedID is the call ID of a set's reversed-order call (RankTwice).
func (j *setRank) reversedID() string { return fmt.Sprintf("S%d-R", j.set.ID) }

// reversedPositions is positions in reverse: the frames shown last-first.
func reversedPositions(positions []int) []int {
	out := make([]int, len(positions))
	for i, p := range positions {
		out[len(positions)-1-i] = p
	}
	return out
}

// takeReversed records the reversed call's answer: its usage, and its order as
// positions, best first. Unlike take it never fails the set (the forward order
// still ranks it) and keeps no notes or summary (the forward call's are used).
func (j *setRank) takeReversed(outs map[string]rankOut, id string, positions []int) ([]int, error) {
	o, ok := outs[id]
	if !ok {
		return nil, nil
	}
	j.usage.Add(o.U)
	if o.key != "" {
		j.keys = append(j.keys, o.key)
	}
	if o.R == nil {
		return nil, o.Err
	}
	order := make([]int, 0, len(o.R.Ranking))
	for _, e := range o.R.Ranking {
		order = append(order, positions[e.Frame-1])
	}
	return order, o.Err
}

// call builds the rank call for the frames at positions, without images.
func (j *setRank) call(id string, positions []int) rankCall {
	c := rankCall{ID: id, job: j, pos: positions}
	for _, p := range positions {
		c.Files = append(c.Files, j.files[p])
	}
	return c
}

// take records the answer to call id, which sent the frames at positions: its
// usage, its notes (a later round's replace an earlier one's), its summary, and
// its order as positions, best first. A missing or failed answer fails the set.
func (j *setRank) take(outs map[string]rankOut, id string, positions []int) ([]int, error) {
	o, ok := outs[id]
	if !ok {
		j.fail(fmt.Errorf("%s: no answer", id))
		return nil, nil
	}
	j.usage.Add(o.U)
	if o.key != "" { // its usage reaches rep when the set is recorded, ranked or not
		j.keys = append(j.keys, o.key)
	}
	if o.R == nil {
		err := o.Err
		if err == nil {
			err = errors.New("empty answer")
		}
		j.fail(fmt.Errorf("%s: %w", id, err))
		return nil, o.Err
	}
	order := make([]int, 0, len(o.R.Ranking))
	for _, e := range o.R.Ranking {
		p := positions[e.Frame-1] // DecodeRank checked the permutation
		order = append(order, p)
		j.notes[p] = e
	}
	j.summary = o.R.Summary
	return order, o.Err
}

// answers runs a round and indexes its answers by call ID.
func answers(ctx context.Context, ex rankExec, calls []rankCall, load loader) map[string]rankOut {
	outs := make(map[string]rankOut, len(calls))
	if len(calls) == 0 {
		return outs
	}
	for _, o := range ex.Run(ctx, calls, load) {
		outs[o.ID] = o
	}
	return outs
}

// finalPositions takes the top finalists(...) of each chunk's order into the final
// round, in capture order. With the chunks themselves as orders it sizes the round.
func finalPositions(parts, orders [][]int, keepBest int) []int {
	f := finalists(len(parts), keepBest)
	var out []int
	for _, o := range orders {
		out = append(out, o[:min(f, len(o))]...)
	}
	sort.Ints(out)
	return out
}

// loadCallImages fills the Frames of calls, decoding each frame they send that
// isn't decoded yet (a set's frames are kept for its final), cfg.Concurrency at a
// time: each decode holds a full-resolution preview. A frame that can't be read
// fails its set, whose calls then get no Frames. Once ctx is cancelled no further
// frame is decoded (those in progress finish), no call gets Frames, and it returns
// ctx's error.
func loadCallImages(ctx context.Context, rep *report.Report, cfg Config, calls []rankCall, byFile map[string]int) error {
	type item struct {
		j   *setRank
		pos int
	}
	var todo []item
	queued := map[item]bool{}
	for _, c := range calls {
		if c.job == nil || c.job.err != nil {
			continue
		}
		for _, p := range c.pos {
			if it := (item{c.job, p}); !c.job.decoded[p] && !queued[it] {
				queued[it] = true
				todo = append(todo, it)
			}
		}
	}
	var mu sync.Mutex
	items := make(chan item)
	var wg sync.WaitGroup
	for w := 0; w < max(1, cfg.Concurrency); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range items {
				if ctx.Err() != nil {
					continue // drain
				}
				f := it.j.files[it.pos]
				rf, err := rankImages(rep.Results[byFile[f]])
				mu.Lock()
				if err != nil {
					it.j.fail(fmt.Errorf("%s: %w", filepath.Base(f), err))
				} else {
					it.j.frames[it.pos], it.j.decoded[it.pos] = rf, true
				}
				mu.Unlock()
			}
		}()
	}
	for _, it := range todo {
		if ctx.Err() != nil {
			break
		}
		items <- it
	}
	close(items)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range calls {
		c := &calls[i]
		if c.job == nil || c.job.err != nil {
			continue
		}
		c.Frames = make([]eval.RankFrame, len(c.pos))
		for k, p := range c.pos {
			c.Frames[k] = c.job.frames[p]
		}
	}
	return nil
}
