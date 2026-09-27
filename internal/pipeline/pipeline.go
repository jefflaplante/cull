// Package pipeline orchestrates extract -> prepare -> evaluate -> decide -> write.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/gophotocull/internal/dng"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
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
	XMPDevelop     bool // also write crs:Exposure2012 / crs:Crop*
	MinPreviewEdge int
	Prep           imageprep.Options
	Policy         eval.Policy
	Model          string
	CheckpointN    int
	Log            io.Writer
}

// Evaluator is the subset of *eval.Client the pipeline needs (mockable in tests).
type Evaluator interface {
	Evaluate(ctx context.Context, in eval.Input) (*eval.Evaluation, eval.Usage, error)
}

func Discover(dir string, recursive bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && !recursive {
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
func Run(ctx context.Context, cfg Config, ev Evaluator) (*report.Report, eval.Usage, error) {
	var total eval.Usage
	files, err := Discover(cfg.Dir, cfg.Recursive)
	if err != nil {
		return nil, total, err
	}
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Model: cfg.Model, Dir: cfg.Dir}
	done := map[string]bool{}
	if cfg.Resume {
		if prev, err := report.Load(cfg.ReportPath); err == nil {
			for _, r := range prev.Results {
				if r.Error == "" && (r.Evaluation != nil || cfg.DryRun) {
					rep.Results = append(rep.Results, r)
					done[r.Key()] = true
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
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
	var wg sync.WaitGroup
	for i := 0; i < max(1, cfg.Concurrency); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				results <- processOne(ctx, cfg, ev, f)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, f := range todo {
			select {
			case jobs <- f:
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
	rep.Generated = time.Now()
	if err := rep.Save(cfg.ReportPath); err != nil {
		return rep, total, err
	}
	return rep, total, ctx.Err()
}

func processOne(ctx context.Context, cfg Config, ev Evaluator, path string) report.Result {
	res := report.Result{File: path}
	if st, err := os.Stat(path); err == nil {
		res.Size, res.ModTime = st.Size(), st.ModTime()
	}
	fail := func(stage string, err error) report.Result {
		res.Error = stage + ": " + err.Error()
		return res
	}

	pv, err := dng.Best(path, cfg.MinPreviewEdge)
	if err != nil {
		return fail("preview", err)
	}
	res.Preview = &report.PreviewInfo{Width: pv.Width, Height: pv.Height, Orientation: pv.Orientation, Source: pv.Source}

	prep, err := imageprep.Prepare(pv.Data, pv.Orientation, cfg.Prep)
	if err != nil {
		return fail("prepare", err)
	}
	res.Stats = &prep.Stats
	if pv.LongEdge() < cfg.MinPreviewEdge {
		res.Fixups = append(res.Fixups, fmt.Sprintf("preview long edge %dpx < %dpx: focus judgement unreliable", pv.LongEdge(), cfg.MinPreviewEdge))
	}
	if cfg.DryRun {
		return res
	}

	e, usage, err := ev.Evaluate(ctx, eval.Input{
		Filename:    filepath.Base(path),
		FullFrame:   prep.FullFrame,
		Tiles:       prep.Tiles,
		StatsText:   statsText(prep),
		MinCropArea: cfg.Policy.MinCropArea,
	})
	res.Usage = usage
	if err != nil {
		return fail("evaluate", err)
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
	return res
}

func statsText(p *imageprep.Prepared) string {
	s := p.Stats
	return fmt.Sprintf(
		"preview %dx%d; mean luma %.1f/255; luma p1/p50/p99 = %d/%d/%d; "+
			"highlight clip (any channel >=250) %.2f%%; shadow clip (luma <=3) %.2f%%; "+
			"Laplacian variance global %.1f, sharpest tile %.1f",
		p.Width, p.Height, s.MeanLuma, s.LumaP1, s.LumaP50, s.LumaP99,
		s.HighlightClipPct, s.ShadowClipPct, s.GlobalSharpness, s.PeakTileSharpness)
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
		return fmt.Sprintf("%s preview %dx%d (%s) peak-sharpness %.0f", name, pv.Width, pv.Height, pv.Source, r.Stats.PeakTileSharpness)
	default:
		e := r.Evaluation
		return fmt.Sprintf("%s %s  sharp %.1f(%s) exp %.1f(%+.1fEV) comp %.1f(%s)",
			name, strings.ToUpper(string(r.Decision)), e.Sharpness.Score, e.Sharpness.Status,
			e.Exposure.Score, e.Exposure.EVAdjust, e.Composition.Score, e.Composition.Status)
	}
}
