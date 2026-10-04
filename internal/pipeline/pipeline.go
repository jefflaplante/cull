// Package pipeline orchestrates extract -> prepare -> evaluate -> decide -> write.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/rawclip"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
)

type Config struct {
	Dir            string
	Recursive      bool
	ReportPath     string
	Concurrency    int
	DryRun         bool // extract + prepare only; no API calls
	// ReviewImages is the review sheet's folder (cull-review beside the report): each
	// frame's thumbnail and subject crop are written into its cache while the full
	// preview is decoded anyway, so review renders nothing. "" = off.
	ReviewImages string
	Resume         bool
	Fresh          bool // replace a report holding assessments; refused while it records moved frames
	WriteXMP       bool
	OverwriteXMP   bool
	MoveCulled     bool                    // move cull decisions (and sidecars) into CulledDir after processing
	Sort           bool                    // move every judged frame (and sidecar) into keep/, review/ or cull/
	Tags           *report.Tags            // given on this command; merged over the folder's stored tags
	Labels         map[string]labels.Entry // your labels by base name: drive moves and sidecar rewrites at the end; nil = the model's
	XMPDevelop     bool                    // also write crs:Exposure2012 / crs:Crop*
	MinPreviewEdge int
	Prep           imageprep.Options
	Policy         eval.Policy
	Backend        string // recorded in the report; resume refuses a mismatch
	Model          string
	LandedTiles    int // "where focus landed" tiles per frame
	// LandedWithSubject also sends landed tiles when there is a subject crop. Off by
	// default: on real frames the model compared smooth skin with crisper fabric in
	// the same plane and called sharp faces missed_focus.
	LandedWithSubject bool
	FaceMinQ          float64       // pigo detection score for a confident face
	Locate            bool          // ask the backend for the focus target when no face is found
	SaveInputs        string        // directory for exactly what the model is sent; "" = off
	Price             *llm.Price    // per-token price of the backend; nil = not billed per token
	Batch             bool          // priced at the batch rate
	MaxCost           float64       // stop dispatch once the run's cost reaches this (USD); 0 = off
	Escalate          *Escalation   // re-evaluate matching frames on a second model; nil = off
	SecondOpinion     bool          // ask the same model again about soft-or-worse frames; disagreement goes to review
	RawClip           bool          // measure highlight clipping in the raw data (~0.8 s/frame)
	BatchPoll         time.Duration // batch mode: time between status checks
	BatchChunkBytes   int           // batch mode: max request bytes per batch (0 = 180 MB)
	Seq               group.Options // sequences of similar frames; Seq.Gap 0 = no grouping
	Rank              bool          // at the end of the run, rank the sets that need it with the run's backend
	RankTokens        int           // max output tokens per rank call; 0 = defaultRankTokens
	RankTwice         bool          // rank each single-call set again with its frames reversed (Set.Reversed)
	Effort            string        // model effort for evaluations and rankings; "" = the model's default
	LocateEffort      string        // the same for locate calls

	rankWith    rankExec                            // set by Run (sync) or RunBatch (batch) when Rank: what finishRun ranks with; nil = no ranking
	pinner      llm.ModelPinner                     // the backend, when its model is an alias: finishRun records what it resolved to
	detect      func(*imageprep.Frame) []focus.Face // test hook; nil = pigo
	CheckpointN int
	Log         io.Writer // Normal-level lines (the CLI backs it with UI)
	UI          ui.Sink   // structured progress events; nil = plain lines through Log
}

// Escalation re-evaluates frames whose first assessment matches On (sharpness
// statuses, or "eyes_closed") on a second, usually stronger, model.
type Escalation struct {
	Backend llm.Backend
	Model   string
	Price   *llm.Price // nil = not billed per token
	On      map[string]bool
}

// Label identifies the escalation target in the report and the resume guard.
func (e *Escalation) Label() string {
	if e == nil {
		return ""
	}
	return e.Backend.Name() + "/" + e.Model
}

func (e *Escalation) matches(ev *eval.Evaluation) bool {
	return e.On[ev.Sharpness.Status] || (e.On["eyes_closed"] && ev.People.Eyes == "closed")
}

func Discover(dir string, recursive bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (!recursive || slices.Contains(placeDirs, d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".dng") && !strings.HasPrefix(d.Name(), "._") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Run processes every DNG and returns the report. The report is checkpointed every
// CheckpointN results so a crash or Ctrl-C loses at most that much paid work.
// When a backend signals llm.ErrQuotaStop or llm.ErrAbortRun, no further frames
// are dispatched; in-flight frames finish and everything done so far is saved.
func Run(ctx context.Context, cfg Config, b llm.Backend) (*report.Report, llm.Usage, error) {
	var total llm.Usage
	if _, err := os.Stat(batchStatePath(cfg)); err == nil {
		return nil, total, fmt.Errorf("an unfinished batch run is recorded in %s: finish it with --batch --resume (or delete that file to start over)", batchStatePath(cfg))
	}
	// Refuse before judging any frame, not after: with ranking on, a sync run
	// whose rank state is still pending a batch would pay to judge again just
	// to pay again to rank the same sets.
	if cfg.Rank {
		if err := RankBatchPending(cfg); err != nil {
			return nil, total, err
		}
	}
	rep, todo, err := startRun(&cfg)
	if err != nil {
		return nil, total, err
	}
	if p, ok := b.(llm.ModelPinner); ok {
		cfg.pinner = p
		if rep.ResolvedModel != "" {
			p.Pin(rep.ResolvedModel)
		}
	}
	if b != nil {
		b = withEffort(b, cfg)
	}
	if cfg.Escalate != nil { // escalated evaluations are evaluations: same effort
		e := *cfg.Escalate
		e.Backend = withEffort(e.Backend, cfg)
		cfg.Escalate = &e
	}

	jobs := make(chan string)
	results := make(chan report.Result)
	stop := make(chan struct{})
	var stopOnce sync.Once
	var stopErr error
	halt := func(err error) { stopOnce.Do(func() { stopErr = err; close(stop) }) }
	var budget spend

	var wg sync.WaitGroup
	for i := 0; i < max(1, cfg.Concurrency); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				select {
				case <-stop:
					continue // drained unprocessed; --resume picks it up
				default:
				}
				res, escUsage, serr := processOne(ctx, cfg, b, f)
				res.CostUSD = cost(cfg, res.Usage, escUsage)
				if serr != nil {
					halt(serr)
				}
				// Checked before the result is handed over, like the quota stop, so no
				// worker picks up another frame once the budget is spent.
				if over, total := budget.add(res.CostUSD, cfg.MaxCost); over {
					halt(fmt.Errorf("%w: $%.2f of $%.2f", llm.ErrBudget, total, cfg.MaxCost))
				}
				results <- res
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, f := range todo {
			select {
			case jobs <- f:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	n := 0
	name := cfg.stageName()
	cfg.stage(ui.Stage{Name: name, Unit: "frames", Total: int64(len(todo))})
	for r := range results {
		n++
		total.Add(r.Usage)
		rep.Results = append(rep.Results, r)
		cfg.frame(name, n, len(todo), r)
		if cfg.CheckpointN > 0 && n%cfg.CheckpointN == 0 {
			rep.Generated = time.Now()
			if err := rep.Save(cfg.ReportPath); err != nil {
				cfg.warn("checkpoint failed: %v", err)
			}
		}
	}
	cfg.stage(ui.Stage{Name: name, Done: true})
	// A stopped run (quota, abort, budget, Ctrl-C) makes no more calls: its sets stay
	// by scores until cull rank.
	if cfg.Rank && !cfg.DryRun && stopErr == nil && ctx.Err() == nil {
		cfg.rankWith = syncExec{b: b, concurrency: cfg.Concurrency, maxTokens: cfg.RankTokens}
	}
	used, err := finishRun(ctx, rep, cfg, &budget)
	total.Add(used)
	if err != nil {
		return rep, total, err
	}
	if stopErr != nil {
		return rep, total, stopErr
	}
	return rep, total, ctx.Err()
}

// processOne evaluates one file. The error return is reserved for conditions that
// must stop the whole run (llm.ErrQuotaStop, llm.ErrAbortRun); per-frame failures
// are recorded in the result.
func processOne(ctx context.Context, cfg Config, b llm.Backend, path string) (report.Result, llm.Usage, error) {
	var escUsage llm.Usage // escalation calls, priced separately
	p, err := prepareFrame(cfg, path)
	if err != nil {
		return p.res, escUsage, nil
	}
	res := &p.res
	if junked(*res, cfg.Policy) { // unmistakably empty: no faces to find, nothing for a model to judge
		cfg.note(ui.Verbose, "  %s junk: %s; not sent to the model", filepath.Base(path), JunkDetail(res.Junk))
		return *res, escUsage, nil
	}
	target, needLocate := faceTarget(cfg, p.frame, res.FocusTarget)
	var stopErr error
	if needLocate {
		small, err := p.frame.Downscaled(locateEdge, 85)
		var loc *eval.LocateResult
		if err == nil {
			var u llm.Usage
			loc, u, err = eval.Locate(ctx, b, small, cameraOf(res.Exif), locateMaxTokens)
			res.Usage.Add(u)
		}
		switch {
		case errors.Is(err, llm.ErrAbortRun):
			res.Error = "locate: " + err.Error()
			return *res, escUsage, err
		case errors.Is(err, llm.ErrQuotaStop):
			// Use the answer if there is one, then stop dispatching. This frame's
			// evaluation still runs, so one call lands past --quota-stop: in-flight
			// work finishes, as everywhere else a stop is signalled.
			stopErr, err = err, nil
		}
		target = applyLocate(res.FocusTarget, loc, err, p.frame)
	}

	in, err := buildInput(cfg, p, target)
	if err != nil {
		res.Error = err.Error()
		return *res, escUsage, stopErr
	}
	if cfg.DryRun {
		return *res, escUsage, nil
	}

	e, usage, err := eval.Evaluate(ctx, b, in)
	res.Usage.Add(usage)
	switch {
	case errors.Is(err, llm.ErrQuotaStop) && e != nil:
		stopErr = err // this frame's result is good; stop dispatching more
	case errors.Is(err, llm.ErrQuotaStop), errors.Is(err, llm.ErrAbortRun):
		res.Error = "evaluate: " + err.Error()
		return *res, escUsage, err
	case err != nil:
		res.Error = "evaluate: " + err.Error()
		return *res, escUsage, stopErr
	}
	if esc := cfg.Escalate; esc != nil && esc.matches(e) {
		e2, u2, err := eval.Evaluate(ctx, esc.Backend, in)
		escUsage.Add(u2)
		res.Usage.Add(u2)
		switch {
		case e2 != nil && (err == nil || errors.Is(err, llm.ErrQuotaStop)):
			res.FirstPass = &report.FirstPass{Backend: b.Name(), Model: cfg.Model, Evaluation: e}
			e = e2
			if err != nil {
				stopErr = err
			}
		case errors.Is(err, llm.ErrAbortRun), errors.Is(err, llm.ErrQuotaStop):
			res.Fixups = append(res.Fixups, "escalation failed: "+err.Error())
			stopErr = err
		default:
			res.Fixups = append(res.Fixups, "escalation failed: "+err.Error())
		}
	}
	if cfg.SecondOpinion && doubtful(e) && stopErr == nil {
		// Same model, same inputs, asked again: its verdicts on borderline frames
		// vary between runs, and a cull should survive a second look. Only the
		// status is used (Facts.Others), so the answer isn't sanitized.
		e2, u2, err := eval.Evaluate(ctx, b, in)
		res.Usage.Add(u2)
		switch {
		case e2 != nil && (err == nil || errors.Is(err, llm.ErrQuotaStop)):
			res.Second = &report.FirstPass{Backend: b.Name(), Model: cfg.Model, Evaluation: e2}
			if err != nil {
				stopErr = err
			}
		case errors.Is(err, llm.ErrAbortRun), errors.Is(err, llm.ErrQuotaStop):
			res.Fixups = append(res.Fixups, "second opinion failed: "+err.Error())
			stopErr = err
		case err != nil:
			res.Fixups = append(res.Fixups, "second opinion failed: "+err.Error())
		}
	}
	finish(cfg, res, e, p.orientation)
	return *res, escUsage, stopErr
}

// doubtful: an assessment a second opinion is worth asking for.
func doubtful(e *eval.Evaluation) bool {
	switch e.Sharpness.Status {
	case "soft", "missed_focus", "motion_blur":
		return true
	}
	return false
}

const (
	locateEdge      = 1024 // the locate call sees a small frame: finding a subject needs no detail
	locateMaxTokens = 256
)

func subjectLabel(ft *report.FocusTarget) string {
	if ft.Source == "face" {
		return "Intended focus target, found by a face detector (centred on the eyes when located), at native preview resolution:"
	}
	return fmt.Sprintf("Intended focus target, located by model: %s. Native preview resolution:", ft.Label)
}

func normBox(r image.Rectangle, w, h int) *eval.NormBox {
	fw, fh := float64(w), float64(h)
	return &eval.NormBox{
		Left: round(float64(r.Min.X)/fw, 4), Top: round(float64(r.Min.Y)/fh, 4),
		Right: round(float64(r.Max.X)/fw, 4), Bottom: round(float64(r.Max.Y)/fh, 4),
	}
}

func denorm(b eval.NormBox, w, h int) image.Rectangle {
	fw, fh := float64(w), float64(h)
	return image.Rect(int(b.Left*fw), int(b.Top*fh), int(math.Ceil(b.Right*fw)), int(math.Ceil(b.Bottom*fh)))
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

const landedLabel = "Region with the most fine detail relative to its local contrast, at native preview resolution: where focus most likely landed."

func statsText(f *imageprep.Frame, s imageprep.Stats, ex *dng.Exif, rc *rawclip.Result) string {
	t := fmt.Sprintf(
		"preview %dx%d; mean luma %.1f/255; luma p1/p50/p99 = %d/%d/%d; "+
			"highlight clip (any channel >=250) %.2f%%; shadow clip (luma <=3) %.2f%%",
		f.W, f.H, s.MeanLuma, s.LumaP1, s.LumaP50, s.LumaP99, s.HighlightClipPct, s.ShadowClipPct)
	if ex != nil {
		if sum := ex.Summary(); sum != "" {
			t += "\nshooting: " + sum
		}
	}
	if rc != nil {
		t += fmt.Sprintf("\nraw data: %.3f%% of samples at the sensor's white level (the preview's clipping overstates this)", rc.HighlightPct)
	}
	return t
}

func summarize(r report.Result) string {
	name := filepath.Base(r.File)
	switch {
	case r.Error != "":
		return name + " ERROR " + r.Error
	case r.Evaluation == nil:
		pv := r.Preview
		return fmt.Sprintf("%s preview %dx%d (%s) %s", name, pv.Width, pv.Height, pv.Source, focusBrief(r.FocusTarget))
	default:
		e := r.Evaluation
		return fmt.Sprintf("%s %s  sharp %.1f(%s) exp %.1f(%+.1fEV) comp %.1f(%s)  %s",
			name, strings.ToUpper(string(r.Decision)), e.Sharpness.Score, e.Sharpness.Status,
			e.Exposure.Score, e.Exposure.EVAdjust, e.Composition.Score, e.Composition.Status, focusBrief(r.FocusTarget))
	}
}

func focusBrief(ft *report.FocusTarget) string {
	switch {
	case ft == nil:
		return ""
	case ft.Source == "face":
		return fmt.Sprintf("[face q=%.0f]", ft.FaceQ)
	case ft.Source == "model":
		return "[model: " + ft.Label + "]"
	}
	return "[no subject: " + ft.Reason + "]"
}

// spend is a run's running cost, shared by the workers.
type spend struct {
	mu    sync.Mutex
	total float64
}

// add records cost and reports whether max (when set) has been reached.
func (s *spend) add(cost, max float64) (bool, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += cost
	return max > 0 && s.total >= max, s.total
}

// cost prices a frame: the primary model for everything but the escalation
// calls, which use the escalation model's price.
func cost(cfg Config, total, esc llm.Usage) float64 {
	primary := total.Sub(esc)
	c := 0.0
	if cfg.Price != nil {
		c += cfg.Price.Cost(primary, cfg.Batch)
	}
	if cfg.Escalate != nil && cfg.Escalate.Price != nil {
		c += cfg.Escalate.Price.Cost(esc, cfg.Batch)
	}
	return c
}

// effortFor sets a request's effort from cfg: locate calls get LocateEffort, the
// rest (evaluations, rankings) Effort.
func (cfg Config) effortFor(req llm.Request) llm.Request {
	if req.SchemaName == "focus_target" {
		req.Effort = cfg.LocateEffort
	} else {
		req.Effort = cfg.Effort
	}
	return req
}

// effortBackend applies cfg's effort to every call it passes on.
type effortBackend struct {
	llm.Backend
	cfg Config
}

func (e effortBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return e.Backend.Call(ctx, e.cfg.effortFor(req))
}

// withEffort wraps b so its calls carry cfg's effort; with none set, b is returned as is.
func withEffort(b llm.Backend, cfg Config) llm.Backend {
	if cfg.Effort == "" && cfg.LocateEffort == "" {
		return b
	}
	return effortBackend{b, cfg}
}

// kept is whether --resume keeps a stored result instead of processing its frame again.
func kept(r report.Result, dryRun bool, p eval.Policy) bool {
	return r.Error == "" && (r.Evaluation != nil || dryRun || junked(r, p))
}

// Pending is the files a judge --resume would still process, given the stored report
// (nil: all of them): what --estimate prices on a resume.
func Pending(files []string, prev *report.Report, p eval.Policy) []string {
	done := map[string]bool{}
	if prev != nil {
		for _, r := range prev.Results {
			if kept(r, false, p) {
				done[r.Key()] = true
			}
		}
	}
	var todo []string
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || !done[report.Key(f, st.Size(), st.ModTime())] {
			todo = append(todo, f)
		}
	}
	return todo
}

// startRun discovers the frames, applies the resume guard, and returns the report
// (holding resumed results) and the frames still to process.
func startRun(cfg *Config) (*report.Report, []string, error) {
	files, err := Discover(cfg.Dir, cfg.Recursive)
	if err != nil {
		return nil, nil, err
	}
	if cfg.detect == nil {
		d, err := focus.NewDetector()
		if err != nil {
			return nil, nil, err
		}
		cfg.detect = func(f *imageprep.Frame) []focus.Face { return d.Detect(f.Luma, f.W, f.H) }
	}
	if !cfg.Resume {
		if err := guardOverwrite(cfg); err != nil {
			return nil, nil, err
		}
	}
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: cfg.Backend, Model: cfg.Model,
		Effort: cfg.Effort, LocateEffort: cfg.LocateEffort, Escalation: cfg.Escalate.Label(), Dir: cfg.Dir}
	done := map[string]bool{}
	// Tags carry over from whatever report the folder has (an offload's scan, an
	// earlier judge), resumed or not; tags given now override them field by field.
	var stored *report.Tags
	if prev, err := report.Load(cfg.ReportPath); err == nil {
		stored = prev.Tags
	}
	rep.Tags = report.MergeTags(stored, cfg.Tags)
	cfg.Tags = rep.Tags // every sidecar this run writes carries the merged tags
	if cfg.Resume {
		prev, err := report.Load(cfg.ReportPath)
		if err == nil && prev.Relocate(cfg.ReportPath, cfg.Dir) {
			fmt.Fprintf(cfg.Log, "the report's frames moved to %s (the folder was renamed): continuing with their new paths\n", cfg.Dir)
		}
		switch {
		case err == nil:
			// Mixing backends or models in one report would corrupt calibration comparisons.
			// An alias report (claude-code "sonnet") also continues under the model ID
			// the alias resolved to: that is what the pin's abort tells the user to
			// pass once the alias has moved on. The report keeps its own name.
			sameModel := prev.Model == cfg.Model || (prev.ResolvedModel != "" && cfg.Model == prev.ResolvedModel)
			if sameModel {
				rep.Model = prev.Model
			}
			if prev.SchemaVersion != report.SchemaVersion || prev.Backend != cfg.Backend || !sameModel ||
				prev.Escalation != rep.Escalation {
				scan := ""
				if prev.Backend == "" {
					scan = " (a scan report)"
				}
				// An older-schema report (e.g. from before sequence ranking) has a free
				// upgrade: cull decide rewrites it in place with no model call. Dropping
				// --resume instead would re-judge, and pay for, the whole shoot again.
				if prev.SchemaVersion < report.SchemaVersion {
					return nil, nil, fmt.Errorf("resume: %s%s was produced by schema v%d; this run is schema v%d: "+
						"run `cull decide %s` (free; it upgrades the report in place), then --resume",
						cfg.ReportPath, scan, prev.SchemaVersion, report.SchemaVersion, cfg.Dir)
				}
				// Dropping --resume is no escape: the overwrite guard refuses it, and
				// --fresh would pay for the shoot again. Name what continues the report.
				fix := fmt.Sprintf("rerun with --backend %s --model %q (and the same --escalate-* flags) to continue it", prev.Backend, prev.Model)
				if prev.Backend == "" {
					fix = "judge it without --resume (a scan report holds nothing paid for)"
				}
				return nil, nil, fmt.Errorf("resume: %s%s was produced by schema v%d, backend %q, model %q, escalation %q; "+
					"this run is schema v%d, backend %q, model %q, escalation %q: %s, or use -o for a separate report",
					cfg.ReportPath, scan, prev.SchemaVersion, prev.Backend, prev.Model, prev.Escalation,
					report.SchemaVersion, cfg.Backend, cfg.Model, rep.Escalation, fix)
			}
			// Effort changes what the model answers, so like the model it can't change
			// within one report: calibration compares reports, not mixtures.
			if prev.Effort != cfg.Effort || prev.LocateEffort != cfg.LocateEffort {
				return nil, nil, fmt.Errorf("resume: %s was judged with --effort %q --locate-effort %q (\"\" = the model's default); "+
					"rerun with those to continue it, or -o for a separate report", cfg.ReportPath, prev.Effort, prev.LocateEffort)
			}
			// Paid rankings carry over: decideAll reuses a stored order while it still
			// covers its set, and ranking spend never leaves the report.
			rep.Sets, rep.RankCostUSD, rep.KeepBest, rep.Policy = prev.Sets, prev.RankCostUSD, prev.KeepBest, prev.Policy
			rep.ResolvedModel = prev.ResolvedModel
			rep.DiscardedCostUSD = prev.DiscardedCostUSD
			for _, r := range prev.Results {
				if kept(r, cfg.DryRun, cfg.Policy) {
					rep.Results = append(rep.Results, r)
					done[r.Key()] = true
					continue
				}
				rep.DiscardedCostUSD += r.CostUSD // re-run below, but what it cost stays paid
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, nil, fmt.Errorf("resume: %w", err)
		}
	}
	var todo []string
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || !done[report.Key(f, st.Size(), st.ModTime())] {
			todo = append(todo, f)
		}
	}
	// A file changed since it was judged (new size or mtime) is in todo again: its
	// old result goes, or the report would hold the frame twice.
	again := make(map[string]bool, len(todo))
	for _, f := range todo {
		again[f] = true
	}
	kept := rep.Results[:0]
	for _, r := range rep.Results {
		if again[r.File] {
			rep.DiscardedCostUSD += r.CostUSD
			continue
		}
		kept = append(kept, r)
	}
	rep.Results = kept
	fmt.Fprintf(cfg.Log, "%d DNGs found, %d already done, %d to process\n", len(files), len(files)-len(todo), len(todo))
	return rep, todo, nil
}

// guardOverwrite refuses a run that would replace a report holding paid
// assessments (unless Fresh) or recording moved frames (always: restore first,
// or those frames can't be found again). A scan-only report is free to replace.
func guardOverwrite(cfg *Config) error {
	prev, err := report.Load(cfg.ReportPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s exists but can't be read (%v): move it aside, or use -o for a separate report", cfg.ReportPath, err)
	}
	prev.Relocate(cfg.ReportPath, cfg.Dir) // in memory only: find its moved frames where they now are
	for i := range prev.Results {          // in memory only: count moves a crashed run never recorded
		reconcileMove(&prev.Results[i])
	}
	evaluated, moved := prev.PaidWork()
	if moved > 0 {
		return fmt.Errorf("%s records %d frame(s) moved into %s/: run `cull restore %s` first, or use --resume or -o",
			cfg.ReportPath, moved, CulledDir, cfg.Dir)
	}
	if evaluated > 0 && !cfg.Fresh {
		return fmt.Errorf("%s holds %d assessed frame(s) ($%.2f): use --resume to continue it, --fresh to replace it, or -o for a separate report",
			cfg.ReportPath, evaluated, prev.Cost())
	}
	return nil
}

// finishRun decides sequences (they span frames, so only once everything is in),
// ranks the sets that need it when cfg.rankWith is set (charging budget, the run's
// spend so far) and decides again, rewrites our sidecars where that changed a
// decision, moves culls when asked, and saves the report. It returns the ranking's
// usage; a ranking stop (budget, quota) is returned after the report is saved.
func finishRun(ctx context.Context, rep *report.Report, cfg Config, budget *spend) (llm.Usage, error) {
	lab := cfg.Labels
	if dups := labels.Duplicates(rep.Results); len(lab) > 0 && len(dups) > 0 {
		cfg.warn("warning: frames share a file name, so your labels can't tell them apart; sidecars and moves follow the model's verdicts: %s", strings.Join(dups, "; "))
		lab = nil
	}
	changed := map[int]bool{}
	for _, i := range decideAll(rep, cfg.Policy, cfg.Seq) {
		changed[i] = true
	}
	var used llm.Usage
	var rankErr error
	ranked := cfg.Rank && cfg.rankWith != nil
	var run rankRun
	if ranked {
		if budget == nil {
			budget = &spend{}
		}
		used, run, rankErr = rankSets(ctx, rep, cfg, cfg.rankWith, false, budget)
		for _, i := range decideAll(rep, cfg.Policy, cfg.Seq) {
			changed[i] = true
		}
	}
	if cfg.WriteXMP {
		o := DecideOptions{XMPDevelop: cfg.XMPDevelop, OverwriteXMP: cfg.OverwriteXMP, Labels: lab, UI: cfg.UI}
		var todo []*report.Result
		for i := range rep.Results {
			if changed[i] {
				todo = append(todo, &rep.Results[i])
			}
		}
		// finish() (stages.go) writes each frame's sidecar during processing, before
		// grouping exists and from the model's verdict alone. Rewrite every frame whose
		// decision didn't change above but that is left in a set (Group != nil, set by
		// decideAll), so its sidecar gets cull:best, or that you labelled, so it carries
		// your verdict and stars as --sort and --move-culled do.
		for i := range rep.Results {
			r := &rep.Results[i]
			if changed[i] || r.Evaluation == nil || r.Error != "" {
				continue
			}
			if _, mine := lab[filepath.Base(r.File)]; r.Group != nil || mine {
				todo = append(todo, r)
			}
		}
		writeSidecars(todo, o, rep.Tags)
	}
	if cfg.pinner != nil {
		if m := cfg.pinner.Resolved(); m != "" {
			rep.ResolvedModel = m
		}
	}
	rep.Generated = time.Now()
	if (cfg.MoveCulled || cfg.Sort) && !cfg.DryRun {
		// Save first: every result is on disk before any frame moves, so a crash
		// mid-move leaves nothing reconcileMove can't find again.
		if err := rep.Save(cfg.ReportPath); err != nil {
			return used, err // the ranking's state stays: a re-run reuses what was paid for
		}
		// After a quota stop or Ctrl-C too: those decisions are final.
		if cfg.Sort {
			n := placeShown(rep, lab, placeSorted, cfg.warnWriter(), true, cfg.UI)
			if n += placeShown(rep, lab, placeSorted, cfg.warnWriter(), false, cfg.UI); n > 0 {
				cfg.note(ui.Quiet, "sorted %d frame(s) into %s/, %s/ and %s/ (undo: cull restore %s)", n, KeepDir, ReviewDir, CullDir, cfg.Dir)
			}
		} else if n := moveCulled(rep, lab, cfg.warnWriter(), cfg.UI); n > 0 {
			cfg.note(ui.Quiet, "moved %d culled frame(s) into %s/ (undo: cull restore %s)", n, CulledDir, cfg.Dir)
		}
	}
	if err := rep.Save(cfg.ReportPath); err != nil {
		return used, err // the ranking's state stays: a re-run reuses what was paid for
	}
	if ranked {
		rankErr = errors.Join(rankErr, cfg.rankWith.commit(run))
	}
	return used, rankErr
}
