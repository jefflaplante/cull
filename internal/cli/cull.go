package cli

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/config"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/pipeline"
	"github.com/jefflaplante/gophotocull/internal/report"
)

type cullOpts struct {
	backend       string
	model         string
	apiKeyFile    string
	baseURL       string
	openaiKeyFile string
	openaiStream  bool
	claudeBin     string
	quotaStop     float64
	locate        string
	concurrency   int
	resume        bool
	writeXMP      bool
	xmpDevelop    bool
	overwriteXMP  bool
	minCropArea   float64
	checkpoint    int
	csv           string
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

Backends (--backend):
  anthropic    Messages API; key from --api-key-file, $ANTHROPIC_API_KEY, then
               ~/.anthropic/api_key, ~/.config/anthropic/api_key, ~/.anthropic_api_key,
               ~/.anthropic. Billed per token.
  claude-code  runs 'claude -p' on your Claude subscription (never an API key).
               Stops cleanly at --quota-stop of the 5-hour window; resume later.
  openai       any OpenAI-compatible server at --base-url (default: a local server on 127.0.0.1:8000).
               --model is required; key optional (--openai-key-file, $OPENAI_API_KEY).`,
		Example: `  gophotocull cull --csv cull.csv ~/Pictures/2026-09-26
  gophotocull cull --backend claude-code -o cc.json ~/Pictures/2026-09-26
  gophotocull cull --backend openai --model <model> ~/Pictures/2026-09-26
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
			if o.concurrency < 0 {
				return fmt.Errorf("--concurrency must be >= 0 (0 = backend default)")
			}
			if _, ok := backendDefaults[o.backend]; !ok {
				return fmt.Errorf("unknown --backend %q (want anthropic, claude-code, or openai)", o.backend)
			}
			if o.backend == "openai" && o.model == "" {
				return fmt.Errorf("--backend openai requires --model (see GET <base-url>/models)")
			}
			if o.quotaStop <= 0 || o.quotaStop > 1 {
				return fmt.Errorf("--quota-stop must be in (0, 1]")
			}
			if o.locate != "model" && o.locate != "off" {
				return fmt.Errorf("--locate must be model or off")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			b, auth, err := o.newBackend(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "backend: %s, model: %s, %s\n", b.Name(), o.model, auth)

			cfg.Backend = b.Name()
			cfg.Model = o.model
			cfg.Locate = o.locate == "model"
			cfg.Concurrency = o.concurrency
			if cfg.Concurrency == 0 {
				cfg.Concurrency = backendDefaults[o.backend].concurrency
			}
			cfg.Resume = o.resume
			cfg.WriteXMP = o.writeXMP
			cfg.XMPDevelop = o.xmpDevelop
			cfg.OverwriteXMP = o.overwriteXMP
			cfg.Policy = eval.Policy{MinCropArea: o.minCropArea}
			cfg.CheckpointN = o.checkpoint

			rep, usage, err := runPipeline(cmd, cfg, b)
			printSummary(cmd, cfg.ReportPath, rep, usage, b.Name())
			if errors.Is(err, llm.ErrQuotaStop) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped early to protect your subscription quota; rerun later with --resume")
			}
			if rep != nil && o.csv != "" {
				if cerr := rep.WriteCSV(o.csv); cerr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "csv:", cerr)
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.backend, "backend", "anthropic", "model backend: anthropic, claude-code, or openai")
	f.StringVarP(&o.model, "model", "m", "", "model (default claude-sonnet-5 for anthropic, sonnet for claude-code; required for openai)")
	f.StringVar(&o.apiKeyFile, "api-key-file", "", "file containing the Anthropic API key")
	f.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:8000/v1", "OpenAI-compatible endpoint (openai backend)")
	f.StringVar(&o.openaiKeyFile, "openai-key-file", "", "file containing a key for the openai backend (optional)")
	f.BoolVar(&o.openaiStream, "openai-stream", true, "stream and hang up once the JSON closes (openai backend)")
	f.StringVar(&o.claudeBin, "claude-bin", "claude", "Claude Code executable (claude-code backend)")
	f.Float64Var(&o.quotaStop, "quota-stop", 0.9, "stop when this fraction of the 5-hour subscription window is used (claude-code backend)")
	f.StringVar(&o.locate, "locate", "model", "when no face is found, ask the model for the focus target: model or off")
	f.IntVarP(&o.concurrency, "concurrency", "j", 0, "parallel evaluations (0 = backend default: anthropic 4, claude-code 2, openai 4)")
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

type backendDefault struct {
	model       string
	concurrency int
	basis       string // how the token counts relate to money
}

var backendDefaults = map[string]backendDefault{
	"anthropic":   {"claude-sonnet-5", 4, "API-billed"},
	"claude-code": {"sonnet", 2, "subscription, not billed per token"},
	"openai":      {"", 4, "OpenAI-compatible server"},
}

// newBackend builds the selected backend and describes its credential source
// without ever printing a key.
func (o *cullOpts) newBackend(cmd *cobra.Command) (llm.Backend, string, error) {
	if o.model == "" {
		o.model = backendDefaults[o.backend].model
	}
	switch o.backend {
	case "claude-code":
		path, err := exec.LookPath(o.claudeBin)
		if err != nil {
			return nil, "", fmt.Errorf("claude binary %q not found: %w", o.claudeBin, err)
		}
		return llm.NewClaudeCode(path, o.model, o.quotaStop), "auth: Claude subscription via " + path, nil
	case "openai":
		key, source, warnings, err := config.LoadOpenAIKey(o.openaiKeyFile)
		if err != nil {
			return nil, "", err
		}
		warn(cmd, warnings)
		b := llm.NewOpenAI(o.baseURL, key, o.model)
		b.Stream = o.openaiStream
		return b, "endpoint: " + b.BaseURL + ", key: " + source, nil
	default:
		key, source, warnings, err := config.LoadAPIKey(o.apiKeyFile)
		if err != nil {
			return nil, "", err
		}
		warn(cmd, warnings)
		return llm.NewAnthropic(key, o.model), "api key: " + source, nil
	}
}

func warn(cmd *cobra.Command, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
}

func runPipeline(cmd *cobra.Command, cfg pipeline.Config, b llm.Backend) (*report.Report, eval.Usage, error) {
	cfg.Log = cmd.ErrOrStderr()
	return pipeline.Run(cmd.Context(), cfg, b)
}

func printSummary(cmd *cobra.Command, path string, rep *report.Report, usage eval.Usage, backend string) {
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
		fmt.Fprintf(cmd.ErrOrStderr(), "tokens this run: in=%d out=%d (%s)\n", usage.InputTokens, usage.OutputTokens, backendDefaults[backend].basis)
	}
}
