// Command gophotocull scores DNGs from their embedded previews with a vision model
// and writes a report (and optionally XMP sidecars) with keep/review/cull decisions.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jefflaplante/gophotocull/internal/config"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/pipeline"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		out         = flag.String("o", "", "report path (default <dir>/gophotocull-report.json)")
		csvPath     = flag.String("csv", "", "also write a CSV summary to this path")
		model       = flag.String("model", "claude-sonnet-5", "Anthropic model")
		keyFile     = flag.String("api-key-file", "", "file containing the API key (default: $ANTHROPIC_API_KEY, then ~/.anthropic/api_key and similar)")
		conc        = flag.Int("concurrency", 4, "parallel evaluations")
		dryRun      = flag.Bool("dry-run", false, "extract and measure previews only; no API calls")
		resume      = flag.Bool("resume", false, "skip files already evaluated in the existing report")
		recursive   = flag.Bool("r", false, "recurse into subdirectories")
		writeXMP    = flag.Bool("write-xmp", false, "write XMP sidecars (rating, label, keyword)")
		xmpDevelop  = flag.Bool("xmp-develop", false, "with -write-xmp, also write Adobe crs exposure/crop (not applied by Capture One)")
		overwrite   = flag.Bool("overwrite-xmp", false, "overwrite existing sidecars (default: never clobber)")
		maxEdge     = flag.Int("max-edge", 1568, "long edge of full-frame image sent to the model")
		tiles       = flag.Int("tiles", 3, "native-resolution detail tiles per image for focus judgement")
		minPreview  = flag.Int("min-preview-edge", 1500, "warn/try exiftool if embedded preview is smaller than this")
		minCrop     = flag.Float64("min-crop-area", 0.6, "reject suggested crops retaining less than this fraction of the frame")
		checkpointN = flag.Int("checkpoint", 25, "save report every N results")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: gophotocull [flags] <dir>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("expected exactly one directory")
	}
	dir, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(dir, "gophotocull-report.json")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ev pipeline.Evaluator
	if !*dryRun {
		key, source, warnings, err := config.LoadAPIKey(*keyFile)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		fmt.Fprintf(os.Stderr, "api key: %s, model: %s\n", source, *model)
		ev = eval.NewClient(key, *model)
	}

	rep, usage, err := pipeline.Run(ctx, pipeline.Config{
		Dir:            dir,
		Recursive:      *recursive,
		ReportPath:     *out,
		Concurrency:    *conc,
		DryRun:         *dryRun,
		Resume:         *resume,
		WriteXMP:       *writeXMP && !*dryRun,
		OverwriteXMP:   *overwrite,
		XMPDevelop:     *xmpDevelop,
		MinPreviewEdge: *minPreview,
		Prep:           imageprep.Options{MaxEdge: *maxEdge, Tiles: *tiles},
		Policy:         eval.Policy{MinCropArea: *minCrop},
		Model:          *model,
		CheckpointN:    *checkpointN,
		Log:            os.Stderr,
	}, ev)
	if rep != nil {
		counts := map[string]int{}
		for _, r := range rep.Results {
			k := string(r.Decision)
			if r.Error != "" {
				k = "error"
			} else if k == "" {
				k = "measured"
			}
			counts[k]++
		}
		fmt.Fprintf(os.Stderr, "\nreport: %s\nresults: %v\ntokens this run: in=%d out=%d\n", *out, counts, usage.InputTokens, usage.OutputTokens)
		if *csvPath != "" {
			if cerr := rep.WriteCSV(*csvPath); cerr != nil {
				fmt.Fprintln(os.Stderr, "csv:", cerr)
			}
		}
	}
	return err
}
