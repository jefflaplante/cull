package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/config"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/pipeline"
	"github.com/jefflaplante/gophotocull/internal/report"
)

type cullOpts struct {
	model        string
	apiKeyFile   string
	concurrency  int
	resume       bool
	writeXMP     bool
	xmpDevelop   bool
	overwriteXMP bool
	minCropArea  float64
	checkpoint   int
	csv          string
}

func newCullCmd(so *sharedOpts) *cobra.Command {
	var o cullOpts
	cmd := &cobra.Command{
		Use:   "cull <dir>",
		Short: "Evaluate every DNG and record keep/review/cull decisions",
		Long: `cull sends each preview (downscaled full frame plus native-resolution detail
tiles) to the model and applies the policy:

  sharpness  missed_focus | motion_blur -> cull, soft -> review
  exposure   fixable -> suggested EV; clipped in preview -> review
  composition never culls; crops retaining < --min-crop-area are dropped

API key: --api-key-file, then $ANTHROPIC_API_KEY, then ~/.anthropic/api_key,
~/.config/anthropic/api_key, ~/.anthropic_api_key, ~/.anthropic.`,
		Example: `  gophotocull cull --csv cull.csv ~/Pictures/2026-09-26
  gophotocull cull --resume --write-xmp ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if o.xmpDevelop && !o.writeXMP {
				return fmt.Errorf("--xmp-develop requires --write-xmp")
			}
			if o.overwriteXMP && !o.writeXMP {
				return fmt.Errorf("--overwrite-xmp requires --write-xmp")
			}
			if o.minCropArea <= 0 || o.minCropArea > 1 {
				return fmt.Errorf("--min-crop-area must be in (0, 1]")
			}
			if o.concurrency < 1 {
				return fmt.Errorf("--concurrency must be >= 1")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			key, source, warnings, err := config.LoadAPIKey(o.apiKeyFile)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "api key: %s, model: %s\n", source, o.model)

			cfg.Model = o.model
			cfg.Concurrency = o.concurrency
			cfg.Resume = o.resume
			cfg.WriteXMP = o.writeXMP
			cfg.XMPDevelop = o.xmpDevelop
			cfg.OverwriteXMP = o.overwriteXMP
			cfg.Policy = eval.Policy{MinCropArea: o.minCropArea}
			cfg.CheckpointN = o.checkpoint

			rep, usage, err := runPipeline(cmd, cfg, eval.NewClient(key, o.model))
			printSummary(cmd, cfg.ReportPath, rep, usage)
			if rep != nil && o.csv != "" {
				if cerr := rep.WriteCSV(o.csv); cerr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "csv:", cerr)
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.model, "model", "m", "claude-sonnet-5", "Anthropic model")
	f.StringVar(&o.apiKeyFile, "api-key-file", "", "file containing the API key")
	f.IntVarP(&o.concurrency, "concurrency", "j", 4, "parallel evaluations")
	f.BoolVar(&o.resume, "resume", false, "skip files already evaluated in the existing report")
	f.BoolVar(&o.writeXMP, "write-xmp", false, "write XMP sidecars (rating, label, keyword)")
	f.BoolVar(&o.xmpDevelop, "xmp-develop", false, "also write Adobe crs exposure/crop (not applied by Capture One)")
	f.BoolVar(&o.overwriteXMP, "overwrite-xmp", false, "overwrite existing sidecars (default: never clobber)")
	f.Float64Var(&o.minCropArea, "min-crop-area", 0.6, "reject suggested crops retaining less than this fraction of the frame")
	f.IntVar(&o.checkpoint, "checkpoint", 25, "save the report every N results")
	f.StringVar(&o.csv, "csv", "", "also write a CSV summary to this path")
	cmd.MarkFlagFilename("api-key-file")
	cmd.MarkFlagFilename("csv", "csv")
	return cmd
}

func runPipeline(cmd *cobra.Command, cfg pipeline.Config, ev pipeline.Evaluator) (*report.Report, eval.Usage, error) {
	cfg.Log = cmd.ErrOrStderr()
	return pipeline.Run(cmd.Context(), cfg, ev)
}

func printSummary(cmd *cobra.Command, path string, rep *report.Report, usage eval.Usage) {
	if rep == nil {
		return
	}
	counts := map[string]int{}
	for _, r := range rep.Results {
		k := string(r.Decision)
		switch {
		case r.Error != "":
			k = "error"
		case k == "":
			k = "measured"
		}
		counts[k]++
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "\nreport: %s\nresults: %v\n", path, counts)
	if usage.InputTokens > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "tokens this run: in=%d out=%d\n", usage.InputTokens, usage.OutputTokens)
	}
}
