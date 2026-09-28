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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/gophotocull/internal/dng"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/rawclip"
	"github.com/jefflaplante/gophotocull/internal/report"
)

type Config struct {
	Dir            string
	Recursive      bool
	ReportPath     string
	Concurrency    int
	DryRun         bool // extract + prepare only; no API calls
	Resume         bool
	WriteXMP       bool
	OverwriteXMP   bool
	MoveCulled     bool                    // move cull decisions (and sidecars) into CulledDir after processing
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
	RawClip           bool          // measure highlight clipping in the raw data (~0.8 s/frame)
	BatchPoll         time.Duration // batch mode: time between status checks
	BatchChunkBytes   int           // batch mode: max request bytes per batch (0 = 180 MB)
	Seq               group.Options // sequences of similar frames; Seq.Gap 0 = no grouping
	Rank              bool          // at the end of the run, rank the sets that need it with the run's backend
	RankTokens        int           // max output tokens per rank call; 0 = defaultRankTokens

	rankWith    rankExec                            // set by Run (sync) or RunBatch (batch) when Rank: what finishRun ranks with; nil = no ranking
	detect      func(*imageprep.Frame) []focus.Face // test hook; nil = pigo
	CheckpointN int
	Log         io.Writer
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
			if p != dir && (!recursive || d.Name() == CulledDir) {
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
	rep, todo, err := startRun(&cfg)
	if err != nil {
		return nil, total, err
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
	for r := range results {
		n++
		total.Add(r.Usage)
		rep.Results = append(rep.Results, r)
		fmt.Fprintf(cfg.Log, "[%d/%d] %s\n", n, len(todo), summarize(r))
		if cfg.CheckpointN > 0 && n%cfg.CheckpointN == 0 {
			rep.Generated = time.Now()
			if err := rep.Save(cfg.ReportPath); err != nil {
				fmt.Fprintf(cfg.Log, "checkpoint failed: %v\n", err)
			}
		}
	}
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
	target, needLocate := faceTarget(cfg, p.frame, res.FocusTarget)
	var stopErr error
	if needLocate {
		small, err := p.frame.Downscaled(locateEdge, 85)
		var loc *eval.LocateResult
		if err == nil {
			var u llm.Usage
			loc, u, err = eval.Locate(ctx, b, small, locateMaxTokens)
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
	finish(cfg, res, e, p.orientation)
	return *res, escUsage, stopErr
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
	primary := llm.Usage{InputTokens: total.InputTokens - esc.InputTokens, OutputTokens: total.OutputTokens - esc.OutputTokens}
	c := 0.0
	if cfg.Price != nil {
		c += cfg.Price.Cost(primary, cfg.Batch)
	}
	if cfg.Escalate != nil && cfg.Escalate.Price != nil {
		c += cfg.Escalate.Price.Cost(esc, cfg.Batch)
	}
	return c
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
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: cfg.Backend, Model: cfg.Model,
		Escalation: cfg.Escalate.Label(), Dir: cfg.Dir}
	done := map[string]bool{}
	if cfg.Resume {
		prev, err := report.Load(cfg.ReportPath)
		switch {
		case err == nil:
			// Mixing backends or models in one report would corrupt calibration comparisons.
			if prev.SchemaVersion != report.SchemaVersion || prev.Backend != cfg.Backend || prev.Model != cfg.Model ||
				prev.Escalation != rep.Escalation {
				scan := ""
				if prev.Backend == "" {
					scan = " (a scan report)"
				}
				return nil, nil, fmt.Errorf("resume: %s%s was produced by schema v%d, backend %q, model %q, escalation %q; "+
					"this run is schema v%d, backend %q, model %q, escalation %q: drop --resume or use -o for a separate report",
					cfg.ReportPath, scan, prev.SchemaVersion, prev.Backend, prev.Model, prev.Escalation,
					report.SchemaVersion, cfg.Backend, cfg.Model, rep.Escalation)
			}
			// Paid rankings carry over: decideAll reuses a stored order while it still
			// covers its set, and ranking spend never leaves the report.
			rep.Sets, rep.RankCostUSD, rep.KeepBest = prev.Sets, prev.RankCostUSD, prev.KeepBest
			for _, r := range prev.Results {
				if r.Error == "" && (r.Evaluation != nil || cfg.DryRun) {
					rep.Results = append(rep.Results, r)
					done[r.Key()] = true
				}
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
	fmt.Fprintf(cfg.Log, "%d DNGs found, %d already done, %d to process\n", len(files), len(files)-len(todo), len(todo))
	return rep, todo, nil
}

// finishRun decides sequences (they span frames, so only once everything is in),
// ranks the sets that need it when cfg.rankWith is set (charging budget, the run's
// spend so far) and decides again, rewrites our sidecars where that changed a
// decision, moves culls when asked, and saves the report. It returns the ranking's
// usage; a ranking stop (budget, quota) is returned after the report is saved.
func finishRun(ctx context.Context, rep *report.Report, cfg Config, budget *spend) (llm.Usage, error) {
	lab := cfg.Labels
	if dups := labels.Duplicates(rep.Results); len(lab) > 0 && len(dups) > 0 {
		fmt.Fprintf(cfg.Log, "warning: frames share a file name, so your labels can't tell them apart; sidecars and moves follow the model's verdicts: %s\n", strings.Join(dups, "; "))
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
		for i := range rep.Results {
			if changed[i] {
				writeDecidedSidecar(&rep.Results[i], DecideOptions{XMPDevelop: cfg.XMPDevelop, OverwriteXMP: cfg.OverwriteXMP, Labels: lab})
			}
		}
	}
	if cfg.MoveCulled && !cfg.DryRun {
		// After a quota stop or Ctrl-C too: those decisions are final.
		if n := moveCulled(rep, lab, cfg.Log); n > 0 {
			fmt.Fprintf(cfg.Log, "moved %d culled frame(s) into %s/ (undo: cull restore %s)\n", n, CulledDir, cfg.Dir)
		}
	}
	rep.Generated = time.Now()
	if err := rep.Save(cfg.ReportPath); err != nil {
		return used, err // the ranking's state stays: a re-run reuses what was paid for
	}
	if ranked {
		rankErr = errors.Join(rankErr, cfg.rankWith.commit(run))
	}
	return used, rankErr
}
