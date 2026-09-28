package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jefflaplante/gophotocull/internal/dng"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// The rank stage: once every frame is judged and grouped, each set of two or more
// rankable frames is compared side by side by the model (in chunks above
// maxRankFrames), and its order is stored in the report's Set for decideAll.

const (
	rankFullEdge      = 768  // long edge of a frame's full view in a rank call
	rankCropMax       = 512  // largest side of its subject crop, native resolution
	defaultRankTokens = 2048 // 8 frames' notes and a summary take a few hundred
)

// rankCall is one rank request, frames in capture order.
type rankCall struct {
	ID     string   // "S<set>"; a chunked set sends "S<set>-C<k>", then "S<set>-F"
	Files  []string // the frames' Result.File, in the same order
	Frames []eval.RankFrame
}

// rankOut answers one rankCall. R can come with an llm.ErrQuotaStop Err: keep it.
type rankOut struct {
	ID  string
	R   *eval.Ranking
	U   eval.Usage
	Err error
}

// rankExec runs one round of rank calls and answers each, in any order (matched
// by ID). syncExec calls a backend; batchExec submits the round as Message Batches.
type rankExec interface {
	Run(ctx context.Context, calls []rankCall) []rankOut
	// batch: calls are billed at the batch rate, and every set goes in one wave
	// (one round of chunk calls, one of finals), since a batch can't be stopped
	// midway. Price and wave shape follow the executor, never cfg.Batch.
	batch() bool
	// done: the ranking finished, every set it started recorded; drop any state
	// kept for re-attaching.
	done() error
}

// syncExec runs rank calls on a backend through a worker pool. After a quota stop
// or an abort it sends no more calls; those answer with the stop as their Err.
type syncExec struct {
	b           llm.Backend
	concurrency int
	maxTokens   int // 0 = defaultRankTokens
}

func (s syncExec) Run(ctx context.Context, calls []rankCall) []rankOut {
	tokens := s.maxTokens
	if tokens <= 0 {
		tokens = defaultRankTokens
	}
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

func (syncExec) batch() bool { return false }
func (syncExec) done() error { return nil }

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

// fillLooks computes the look of every measured frame that has none (a schema-v3
// report) from its preview, read from where the frame lives now. A frame that
// can't be read keeps none, so it joins no set. It returns how many it filled; once
// ctx is cancelled it stops between frames with ctx's error, leaving the rest
// without a look (which decideAll takes: they join no set).
func fillLooks(ctx context.Context, rep *report.Report) (int, error) {
	n := 0
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.Look != "" || r.Error != "" || r.Preview == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		f, err := decodeWhereItLives(*r)
		if err != nil {
			continue
		}
		r.Look = report.EncodeLook(f.Grid(group.LookSize))
		n++
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
func Rank(ctx context.Context, cfg Config, b llm.Backend, force bool) (*report.Report, error) {
	log := cfg.Log
	if log == nil {
		log = io.Discard
	}
	rep, err := report.Load(cfg.ReportPath)
	if err != nil {
		return nil, err
	}
	n, err := fillLooks(ctx, rep)
	if n > 0 {
		fmt.Fprintf(log, "computed the look of %d frame(s) from their DNGs\n", n)
	}
	if err != nil { // Ctrl-C: keep the looks computed so far (free but slow); decide and rank nothing
		if n > 0 {
			err = errors.Join(err, rep.Save(cfg.ReportPath))
		}
		return rep, err
	}
	o := DecideOptions{Policy: cfg.Policy, WriteXMP: cfg.WriteXMP, XMPDevelop: cfg.XMPDevelop, OverwriteXMP: cfg.OverwriteXMP,
		MoveCulled: cfg.MoveCulled, Seq: cfg.Seq, Labels: cfg.Labels}
	ex := syncExec{b: b, concurrency: cfg.Concurrency, maxTokens: cfg.RankTokens}
	var rankErr error
	if _, err := redecide(rep, o, log, func() { rankErr = RankSets(ctx, rep, cfg, ex, force) }); err != nil {
		return rep, err
	}
	if err := rep.Save(cfg.ReportPath); err != nil {
		return rep, err
	}
	return rep, rankErr
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
func RankSets(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, force bool) error {
	_, err := rankSets(ctx, rep, cfg, ex, force, &spend{})
	return err
}

// rankSets is RankSets charging a run's budget, which may already hold the cost of
// the run's judge calls. It returns the ranking's usage.
func rankSets(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, force bool, budget *spend) (llm.Usage, error) {
	var total llm.Usage
	log := cfg.Log
	if log == nil {
		log = io.Discard
	}
	todo := needsRanking(rep)
	if force {
		todo = nil
		for i, s := range rep.Sets {
			if s.Of >= 2 {
				todo = append(todo, i)
			}
		}
	}
	if len(todo) > 0 && !ex.batch() {
		if p := rankBatchStatePath(cfg); fileExists(p) {
			return total, fmt.Errorf("an unfinished batch ranking is recorded in %s: rerun with --batch to re-attach to it "+
				"(ranking without it would pay for the same sets again), or delete that file to start over", p)
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
	for len(todo) > 0 {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if over, spent := budget.add(0, cfg.MaxCost); over {
			return total, fmt.Errorf("%w: $%.2f of $%.2f; %d set(s) left by scores", llm.ErrBudget, spent, cfg.MaxCost, len(todo))
		}
		n := min(wave, len(todo))
		u, err := rankWave(ctx, rep, cfg, ex, todo[:n], byFile, budget, log)
		total.Add(u)
		todo = todo[n:]
		switch {
		case err == nil:
		case errors.Is(err, llm.ErrBudget) && len(todo) > 0:
			return total, fmt.Errorf("%w; %d set(s) left by scores", err, len(todo))
		case errors.Is(err, llm.ErrBudget): // the last wave is recorded: the ranking finished at the budget
			return total, errors.Join(err, ex.done())
		default:
			return total, err
		}
	}
	if err := ex.done(); err != nil {
		return total, err
	}
	return total, ctx.Err()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// setRank is one set's ranking in progress. Positions index files and frames.
type setRank struct {
	set     *report.Set
	files   []string // rankable members, capture order
	frames  []eval.RankFrame
	parts   [][]int // chunks of positions: one part = a single call
	orders  [][]int // each part's answer, positions best first
	final   []int   // positions sent to the final call, capture order
	order   []int   // the set's order, best first
	notes   map[int]eval.RankEntry
	summary string
	usage   llm.Usage
	err     error
}

func (j *setRank) fail(err error) {
	if j.err == nil {
		j.err = err
	}
}

// rankWave ranks a group of sets: their images, round 1 (a single call per set, or
// its chunk calls), round 2 (the chunked sets' finals), then records each set.
func rankWave(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, sets []int, byFile map[string]int, budget *spend, log io.Writer) (llm.Usage, error) {
	var jobs []*setRank
	for _, si := range sets {
		j := &setRank{set: &rep.Sets[si], notes: map[int]eval.RankEntry{}}
		for _, f := range j.set.Members {
			if i, ok := byFile[f]; ok && rep.Results[i].Group != nil && rep.Results[i].Group.Rank > 0 {
				j.files = append(j.files, f)
			}
		}
		if len(j.files) < 2 {
			continue // nothing to compare (groups not decided)
		}
		j.parts = chunks(len(j.files))
		if len(j.parts) > 1 {
			if n := len(finalPositions(j.parts, j.parts, cfg.Policy.KeepBest)); n > maxRankFrames {
				j.fail(fmt.Errorf("its final round would compare %d frames, more than %d", n, maxRankFrames))
			}
		}
		jobs = append(jobs, j)
	}
	if err := loadRankImages(ctx, rep, cfg, jobs, byFile); err != nil {
		return llm.Usage{}, err // cancelled before any call: nothing spent, no set touched
	}

	// Round 1.
	var calls []rankCall
	for _, j := range jobs {
		if j.err != nil {
			continue
		}
		for k, part := range j.parts {
			calls = append(calls, j.call(j.partID(k), part))
		}
	}
	outs := answers(ctx, ex, calls)
	var stop error
	for _, j := range jobs {
		if j.err != nil {
			continue
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
		outs = answers(ctx, ex, calls)
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
	// A batch executor re-attaches to a round only when the same calls come back.
	// With a round left pending, rank none of the wave: the re-run then sends the
	// same round 1 (answered from its state, not paid again) and finds round 2.
	if errors.Is(stop, errBatchPending) {
		for _, j := range jobs {
			j.fail(stop)
		}
	}

	// Record: every set pays for its calls; a complete answer ranks it.
	var total llm.Usage
	over, spent := false, 0.0
	for _, j := range jobs {
		s := j.set
		c := rankPrice(cfg, j.usage, ex.batch())
		s.Usage.Add(j.usage)
		s.CostUSD += c
		rep.RankCostUSD += c
		total.Add(j.usage)
		if o, t := budget.add(c, cfg.MaxCost); o {
			over, spent = true, t
		}
		if j.err != nil {
			if !errors.Is(j.err, errBatchPending) { // the stage's error says it once
				fmt.Fprintf(log, "set %d (%d frames) not ranked: %v\n", s.ID, len(j.files), j.err)
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
		s.Summary, s.By = j.summary, "model"
		fmt.Fprintf(log, "ranked set %d (%d frames): %s wins — %s\n", s.ID, len(j.files), filepath.Base(s.Order[0]), s.Summary)
	}
	switch {
	case stop != nil:
		return total, stop
	case over:
		return total, fmt.Errorf("%w: $%.2f of $%.2f", llm.ErrBudget, spent, cfg.MaxCost)
	}
	return total, nil
}

// partID is the call ID of round-1 part k: "S<set>" when the set fits one call.
func (j *setRank) partID(k int) string {
	if len(j.parts) == 1 {
		return fmt.Sprintf("S%d", j.set.ID)
	}
	return fmt.Sprintf("S%d-C%d", j.set.ID, k+1)
}

// call builds the rank call for the frames at positions.
func (j *setRank) call(id string, positions []int) rankCall {
	c := rankCall{ID: id}
	for _, p := range positions {
		c.Files = append(c.Files, j.files[p])
		c.Frames = append(c.Frames, j.frames[p])
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
func answers(ctx context.Context, ex rankExec, calls []rankCall) map[string]rankOut {
	outs := make(map[string]rankOut, len(calls))
	if len(calls) == 0 {
		return outs
	}
	for _, o := range ex.Run(ctx, calls) {
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

// loadRankImages reads every frame the wave's sets send, cfg.Concurrency frames at
// a time: each decode holds a full-resolution preview. Once ctx is cancelled no
// further frame is decoded (those in progress finish) and it returns ctx's error.
func loadRankImages(ctx context.Context, rep *report.Report, cfg Config, jobs []*setRank, byFile map[string]int) error {
	type item struct {
		j   *setRank
		pos int
	}
	for _, j := range jobs {
		j.frames = make([]eval.RankFrame, len(j.files))
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
				}
				it.j.frames[it.pos] = rf
				mu.Unlock()
			}
		}()
	}
send:
	for _, j := range jobs {
		if j.err != nil {
			continue
		}
		for pos := range j.files {
			if ctx.Err() != nil {
				break send
			}
			items <- item{j, pos}
		}
	}
	close(items)
	wg.Wait()
	return ctx.Err()
}

// batchExec runs each round of rank calls as Message Batches (half price,
// asynchronous). It records a round's batches in statePath before polling them, so
// a re-run that sends the same round re-attaches instead of paying again.
type batchExec struct {
	client    BatchClient
	cfg       Config
	statePath string
}

// errBatchPending marks a rank round a batch executor couldn't finish (submit
// outcome unknown, polling failed, Ctrl-C): what it sent stays recorded.
var errBatchPending = errors.New("batch ranking unfinished")

const rankBatchStateVersion = 1

func rankBatchStatePath(cfg Config) string { return cfg.ReportPath + ".rank-batch.json" }

// rankBatchState is a ranking's batches, for re-attaching after a crash or Ctrl-C.
type rankBatchState struct {
	Version int          `json:"version"`
	Backend string       `json:"backend"`
	Model   string       `json:"model"`
	Rounds  []*rankRound `json:"rounds"`
}

// rankRound is one round of rank calls: its key (roundKey), its batches (a round
// too big for one upload is split, as judge's are) and, once every batch is
// collected, the answers by call ID.
type rankRound struct {
	Key     string                 `json:"key"`
	Batches []*batchRecord         `json:"batches"`
	Answers map[string]*rankAnswer `json:"answers,omitempty"`
}

type rankAnswer struct {
	Ranking *eval.Ranking `json:"ranking,omitempty"`
	Usage   llm.Usage     `json:"usage"`
	Err     string        `json:"error,omitempty"`
}

func (batchExec) batch() bool { return true }

func (e batchExec) log() io.Writer {
	if e.cfg.Log == nil {
		return io.Discard
	}
	return e.cfg.Log
}

// Run answers a round of calls. When the round can't finish, every call answers
// with the same errBatchPending error, which stops the stage.
func (e batchExec) Run(ctx context.Context, calls []rankCall) []rankOut {
	answers, fresh, err := e.round(ctx, calls)
	out := make([]rankOut, len(calls))
	for i, c := range calls {
		out[i].ID = c.ID
		a := answers[c.ID]
		switch {
		case err != nil:
			out[i].Err = err
		case a == nil:
			out[i].Err = errors.New("no answer in its batch")
		default:
			out[i].R = a.Ranking
			if fresh { // an answer read back from the state was charged when first handed over
				out[i].U = a.Usage
			}
			if a.Err != "" {
				out[i].Err = errors.New(a.Err)
			}
		}
	}
	return out
}

// round answers a round of calls. A round already collected is answered from the
// state (fresh false). Otherwise it submits the calls none of the round's recorded
// batches holds, polls every batch until it ends, and hands the answers over only
// once all are in: a partly collected round is fetched again on the re-run.
func (e batchExec) round(ctx context.Context, calls []rankCall) (answers map[string]*rankAnswer, fresh bool, err error) {
	cfg := e.cfg
	cfg.Log = e.log()
	st, err := loadRankBatchState(e.statePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st = &rankBatchState{Version: rankBatchStateVersion, Backend: cfg.Backend, Model: cfg.Model}
	case err != nil:
		return nil, false, fmt.Errorf("%w: %w", errBatchPending, err)
	case st.Backend != cfg.Backend || st.Model != cfg.Model:
		return nil, false, fmt.Errorf("%w: %s belongs to %s/%s, not %s/%s: rerun with that backend and model to re-attach, or delete it to start over",
			errBatchPending, e.statePath, st.Backend, st.Model, cfg.Backend, cfg.Model)
	}
	key := roundKey(calls)
	var rd *rankRound
	n := 0 // the round's place in the state, for the log
	for i, r := range st.Rounds {
		if r.Key == key {
			rd, n = r, i+1
		}
	}
	if rd != nil && rd.Answers != nil {
		fmt.Fprintf(cfg.Log, "rank round %d: answers recorded in %s\n", n, e.statePath)
		return rd.Answers, false, nil
	}
	if rd == nil {
		rd = &rankRound{Key: key}
		st.Rounds = append(st.Rounds, rd)
		n = len(st.Rounds)
	}
	save := func() error { return saveState(e.statePath, st) }
	pending := func(err error) error {
		return fmt.Errorf("%w: %w (recorded in %s: rerun with --batch to re-attach)", errBatchPending, err, e.statePath)
	}
	sent := map[string]bool{}
	for _, b := range rd.Batches {
		if b.Status == "submitting" {
			return nil, false, fmt.Errorf("%w: a rank batch submission was interrupted before its ID was recorded, so it may have been created: "+
				"check the Batches page in the Claude Console, then delete %s to start over (not resubmitting, to avoid paying twice)", errBatchPending, e.statePath)
		}
		for _, id := range b.CustomIDs {
			sent[id] = true
		}
	}
	tokens := cfg.RankTokens
	if tokens <= 0 {
		tokens = defaultRankTokens
	}
	frames := make(map[string]int, len(calls)) // call ID -> frames it sent
	var reqs []llm.BatchRequest
	for _, c := range calls {
		frames[c.ID] = len(c.Frames)
		if !sent[c.ID] {
			reqs = append(reqs, llm.BatchRequest{CustomID: c.ID, Req: eval.RankRequest(c.Frames, tokens)})
		}
	}
	if err := submit(ctx, cfg, e.client, &rd.Batches, reqs, "rank", n, save); err != nil {
		return nil, false, pending(err)
	}
	answers = make(map[string]*rankAnswer, len(calls))
	schemaFor := func(string) map[string]any { return eval.RankSchema() }
	for _, b := range rd.Batches {
		status, err := await(ctx, cfg, e.client, b.ID)
		if err != nil {
			return nil, false, pending(err)
		}
		err = e.client.BatchResults(ctx, status.ResultsURL, schemaFor, func(r llm.BatchResult) {
			if nf, ok := frames[r.CustomID]; ok {
				answers[r.CustomID] = rankAnswerOf(r, nf)
			}
		})
		if err != nil {
			return nil, false, pending(fmt.Errorf("batch %s results: %w", b.ID, err))
		}
	}
	rd.Answers = answers
	for _, b := range rd.Batches {
		b.Status = "collected"
	}
	if err := save(); err != nil {
		return nil, false, pending(err) // not handed over: the re-run fetches them again
	}
	return answers, true, nil
}

// rankAnswerOf reads one batch result for a call that sent n frames. An answer
// that isn't a clean order is that call's error: a batch has no retry.
func rankAnswerOf(r llm.BatchResult, n int) *rankAnswer {
	a := &rankAnswer{}
	if r.Response != nil {
		a.Usage = r.Response.Usage
	}
	switch {
	case r.Err != nil:
		a.Err = r.Err.Error()
	case r.Response == nil:
		a.Err = "empty answer"
	default:
		rk, err := eval.DecodeRank(r.Response.JSON, n)
		if err != nil {
			a.Err = err.Error()
		} else {
			a.Ranking = rk
		}
	}
	return a
}

// done deletes the state: the ranking finished, so every round it recorded was
// applied. A batch never collected belonged to sets that changed meanwhile; it is
// named in the log, since it may still be billed.
func (e batchExec) done() error {
	st, err := loadRankBatchState(e.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		for _, rd := range st.Rounds {
			for _, b := range rd.Batches {
				if b.Status != "collected" {
					fmt.Fprintf(e.log(), "dropping rank batch %s (%s): its sets changed before it was collected\n", b.ID, b.Status)
				}
			}
		}
	}
	if err := os.Remove(e.statePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// roundKey identifies a round by its calls: their IDs, sorted, each with its frame
// files in the order sent. The same sets needing the same calls give the same key.
func roundKey(calls []rankCall) string {
	sorted := append([]rankCall(nil), calls...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	h := sha256.New()
	for _, c := range sorted {
		fmt.Fprintf(h, "%s\x00%s\n", c.ID, strings.Join(c.Files, "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func loadRankBatchState(path string) (*rankBatchState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st rankBatchState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}
