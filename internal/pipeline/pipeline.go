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
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
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
	MoveCulled     bool // move cull decisions (and sidecars) into CulledDir after processing
	XMPDevelop     bool // also write crs:Exposure2012 / crs:Crop*
	MinPreviewEdge int
	Prep           imageprep.Options
	Policy         eval.Policy
	Backend        string // recorded in the report; resume refuses a mismatch
	Model          string
	LandedTiles    int     // "where focus landed" tiles per frame
	FaceMinQ       float64 // pigo detection score for a confident face
	Locate         bool    // ask the backend for the focus target when no face is found
	SaveInputs     string  // directory for exactly what the model is sent; "" = off

	detect      func(*imageprep.Frame) []focus.Face // test hook; nil = pigo
	CheckpointN int
	Log         io.Writer
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
	files, err := Discover(cfg.Dir, cfg.Recursive)
	if err != nil {
		return nil, total, err
	}
	if cfg.detect == nil {
		d, err := focus.NewDetector()
		if err != nil {
			return nil, total, err
		}
		cfg.detect = func(f *imageprep.Frame) []focus.Face { return d.Detect(f.Luma, f.W, f.H) }
	}
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: cfg.Backend, Model: cfg.Model, Dir: cfg.Dir}
	done := map[string]bool{}
	if cfg.Resume {
		prev, err := report.Load(cfg.ReportPath)
		switch {
		case err == nil:
			// Mixing backends or models in one report would corrupt calibration comparisons.
			if prev.SchemaVersion != report.SchemaVersion || prev.Backend != cfg.Backend || prev.Model != cfg.Model {
				return nil, total, fmt.Errorf("resume: %s was produced by schema v%d, backend %q, model %q; "+
					"this run is schema v%d, backend %q, model %q: drop --resume or use -o for a separate report",
					cfg.ReportPath, prev.SchemaVersion, prev.Backend, prev.Model, report.SchemaVersion, cfg.Backend, cfg.Model)
			}
			for _, r := range prev.Results {
				if r.Error == "" && (r.Evaluation != nil || cfg.DryRun) {
					rep.Results = append(rep.Results, r)
					done[r.Key()] = true
				}
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, total, fmt.Errorf("resume: %w", err)
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

	jobs := make(chan string)
	results := make(chan report.Result)
	stop := make(chan struct{})
	var stopOnce sync.Once
	var stopErr error
	halt := func(err error) { stopOnce.Do(func() { stopErr = err; close(stop) }) }

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
				res, serr := processOne(ctx, cfg, b, f)
				if serr != nil {
					halt(serr)
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
	if cfg.MoveCulled && !cfg.DryRun {
		// After a quota stop or Ctrl-C too: those decisions are final.
		if n := moveCulled(rep, cfg.Log); n > 0 {
			fmt.Fprintf(cfg.Log, "moved %d culled frame(s) into %s/ (undo: gophotocull restore %s)\n", n, CulledDir, cfg.Dir)
		}
	}
	rep.Generated = time.Now()
	if err := rep.Save(cfg.ReportPath); err != nil {
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
func processOne(ctx context.Context, cfg Config, b llm.Backend, path string) (report.Result, error) {
	res := report.Result{File: path}
	if st, err := os.Stat(path); err == nil {
		res.Size, res.ModTime = st.Size(), st.ModTime()
	}
	fail := func(stage string, err error) report.Result {
		res.Error = stage + ": " + err.Error()
		return res
	}
	var stopErr error

	pv, err := dng.Best(path, cfg.MinPreviewEdge)
	if err != nil {
		return fail("preview", err), nil
	}
	res.Preview = &report.PreviewInfo{Width: pv.Width, Height: pv.Height, Orientation: pv.Orientation, Source: pv.Source}

	frame, err := imageprep.Decode(pv.Data, pv.Orientation)
	if err != nil {
		return fail("decode", err), nil
	}
	stats := imageprep.Measure(frame)
	res.Stats = &stats
	if pv.LongEdge() < cfg.MinPreviewEdge {
		res.Fixups = append(res.Fixups, fmt.Sprintf("preview long edge %dpx < %dpx: focus judgement unreliable", pv.LongEdge(), cfg.MinPreviewEdge))
	}

	ft := &report.FocusTarget{}
	res.FocusTarget = ft
	target, serr := locateTarget(ctx, cfg, b, frame, ft, &res.Usage)
	if errors.Is(serr, llm.ErrAbortRun) {
		return fail("locate", serr), serr
	}
	stopErr = serr

	var subjectRect image.Rectangle
	if target != nil {
		subjectRect = focus.SubjectRect(*target, frame.W, frame.H)
	}
	cells, noise := focus.Landed(frame.Luma, frame.W, frame.H, subjectRect, cfg.LandedTiles)
	if len(cells) > 0 {
		ft.LandedSharpness = round(cells[0].Ratio, 3)
	}
	var subject *eval.Labeled
	if target != nil {
		ft.SubjectSharpness = round(focus.Ratio(frame.Luma, frame.W, subjectRect, noise), 3)
		jb, err := frame.Crop(subjectRect, 90)
		if err != nil {
			return fail("crop", err), stopErr
		}
		subject = &eval.Labeled{Label: subjectLabel(ft), JPEG: jb}
	}
	var landed []eval.Labeled
	for _, c := range cells {
		jb, err := frame.Crop(c.Crop, 90)
		if err != nil {
			return fail("crop", err), stopErr
		}
		landed = append(landed, eval.Labeled{Label: landedLabel, JPEG: jb})
	}
	if cfg.DryRun && cfg.SaveInputs == "" {
		return res, nil
	}
	full, err := frame.Downscaled(cfg.Prep.MaxEdge, 85)
	if err != nil {
		return fail("encode", err), stopErr
	}
	in := eval.Input{
		Filename:    filepath.Base(path),
		FullFrame:   full,
		Subject:     subject,
		Landed:      landed,
		StatsText:   statsText(frame, stats),
		MinCropArea: cfg.Policy.MinCropArea,
	}
	if cfg.SaveInputs != "" {
		names := []string{"full"}
		if subject != nil {
			names = append(names, "subject")
		}
		for i := range landed {
			names = append(names, fmt.Sprintf("landed-%d", i+1))
		}
		if err := saveInputs(cfg.SaveInputs, inputsBase(cfg.Dir, path), eval.EvalRequest(in), names, ft); err != nil {
			res.Fixups = append(res.Fixups, "save-inputs: "+err.Error())
		}
	}
	if cfg.DryRun {
		return res, nil
	}

	e, usage, err := eval.Evaluate(ctx, b, in)
	res.Usage.Add(usage)
	switch {
	case errors.Is(err, llm.ErrQuotaStop) && e != nil:
		stopErr = err // this frame's result is good; stop dispatching more
	case errors.Is(err, llm.ErrQuotaStop), errors.Is(err, llm.ErrAbortRun):
		return fail("evaluate", err), err
	case err != nil:
		return fail("evaluate", err), stopErr
	}
	res.Fixups = append(res.Fixups, cfg.Policy.Sanitize(e)...)
	res.Evaluation = e
	res.Decision, res.Reasons = cfg.Policy.Decide(e)

	if cfg.WriteXMP {
		sc := buildSidecar(res, pv.Orientation, cfg.XMPDevelop)
		p := xmp.Path(path)
		switch err := xmp.Write(p, sc, cfg.OverwriteXMP); {
		case err == nil:
			res.XMP = p
		case errors.Is(err, xmp.ErrExists):
			res.Fixups = append(res.Fixups, "xmp: sidecar exists, not overwritten")
		default:
			res.Fixups = append(res.Fixups, "xmp: "+err.Error())
		}
	}
	return res, stopErr
}

const (
	locateEdge      = 1024 // the locate call sees a small frame: finding a subject needs no detail
	locateMaxTokens = 256
)

// locateTarget decides what should be sharp: the most confident face, else (for
// cull with --locate model) the subject the model points at. It fills ft,
// including why there is no target. The error is reserved for run-stopping
// conditions (llm.ErrAbortRun, llm.ErrQuotaStop); a failed locate call only
// means "no subject crop" and the frame is still evaluated.
func locateTarget(ctx context.Context, cfg Config, b llm.Backend, frame *imageprep.Frame, ft *report.FocusTarget, usage *llm.Usage) (*focus.Target, error) {
	faces := focus.Confident(cfg.detect(frame), cfg.FaceMinQ)
	ft.Faces = len(faces)
	if len(faces) > 0 {
		t := focus.FaceTarget(faces[0])
		ft.Source, ft.FaceQ, ft.Box = "face", round(faces[0].Q, 1), normBox(faces[0].Rect, frame.W, frame.H)
		return &t, nil
	}
	ft.Source = "none"
	switch {
	case cfg.DryRun:
		ft.Reason = "no face; scan does not call a model"
		return nil, nil
	case !cfg.Locate:
		ft.Reason = "no face; locate off"
		return nil, nil
	}
	small, err := frame.Downscaled(locateEdge, 85)
	if err != nil {
		ft.Reason = "locate failed: " + err.Error()
		return nil, nil
	}
	loc, u, err := eval.Locate(ctx, b, small, locateMaxTokens)
	usage.Add(u)
	var stop error
	switch {
	case errors.Is(err, llm.ErrAbortRun):
		return nil, err
	case errors.Is(err, llm.ErrQuotaStop):
		stop = err // use the answer if there is one, then stop dispatching
	case err != nil:
		ft.Reason = "locate failed: " + err.Error()
		return nil, nil
	}
	switch {
	case loc == nil:
		ft.Reason = "locate failed: no result"
	case !loc.Confident || loc.Kind == "none":
		ft.Reason = "model: no clear subject"
	case !loc.Box.Valid():
		ft.Reason = fmt.Sprintf("model: invalid box %+v", loc.Box)
	default:
		box := loc.Box
		t := focus.BoxTarget(denorm(box, frame.W, frame.H))
		ft.Source, ft.Box, ft.Label = "model", &box, loc.Subject
		return &t, stop
	}
	return nil, stop
}

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

func statsText(f *imageprep.Frame, s imageprep.Stats) string {
	return fmt.Sprintf(
		"preview %dx%d; mean luma %.1f/255; luma p1/p50/p99 = %d/%d/%d; "+
			"highlight clip (any channel >=250) %.2f%%; shadow clip (luma <=3) %.2f%%",
		f.W, f.H, s.MeanLuma, s.LumaP1, s.LumaP50, s.LumaP99, s.HighlightClipPct, s.ShadowClipPct)
}

// buildSidecar maps a decision to metadata. Ratings: keep=3, review=2, cull=1.
func buildSidecar(r report.Result, orientation int, develop bool) xmp.Sidecar {
	sc := xmp.Sidecar{Keywords: []string{"gophotocull:" + string(r.Decision)}}
	switch r.Decision {
	case eval.Keep:
		sc.Rating = 3
	case eval.Review:
		sc.Rating, sc.Label = 2, "Yellow"
	case eval.Cull:
		sc.Rating, sc.Label = 1, "Red"
	}
	if !develop || r.Evaluation == nil || r.Decision == eval.Cull {
		return sc
	}
	e := r.Evaluation
	if e.Exposure.Status == "fixable" {
		v := e.Exposure.EVAdjust
		sc.ExposureEV = &v
	}
	if c := e.Composition.Crop; c.Apply {
		b := xmp.FromDisplay(c.Left, c.Top, c.Right, c.Bottom, orientation)
		sc.Crop = &b
	}
	return sc
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
