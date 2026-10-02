package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

type cullOpts struct {
	backendFlags
	locate       string
	concurrency  int
	resume       bool
	fresh        bool
	writeXMP     bool
	xmpDevelop   bool
	overwriteXMP bool
	moveCulled   bool
	noLabels     bool
	labelsPath   string
	rawClip      bool
	batch        bool
	batchPoll    time.Duration
	estimate     bool
	maxCost      float64
	noRank       bool
	second       bool
	rankTwice    bool
	yes          bool

	escalateBackend string
	escalateModel   string
	escalateOnList  string
	policy          policyFlags
	checkpoint      int
}

func newCullCmd(so *sharedOpts) *cobra.Command {
	var o cullOpts
	cmd := &cobra.Command{
		Use:        "judge <dir>",
		SuggestFor: []string{"cull"}, // the command was 'cull cull' before the binary became cull
		Short:      "Evaluate every DNG and record keep/review/cull decisions",
		Long: `judge sends each preview (downscaled full frame plus native-resolution detail
tiles) to the model and applies the policy:

  sharpness  missed_focus | motion_blur -> cull, soft -> review
  exposure   fixable -> suggested EV; clipped in preview -> review
  composition never culls; crops retaining < --min-crop-area are dropped

Frames judged similar (a sequence: --seq-gap, --seq-look) are grouped into a set and,
once every frame in it is judged, ranked with one model call (more for large sets,
chunked and merged): the --keep-best best of each set keep their decision, the rest
get --outranked. --no-rank skips ranking (run it later with 'cull rank').

Backends (--backend):
  anthropic    Messages API; key from --api-key-file, $ANTHROPIC_API_KEY, then
               ~/.anthropic/api_key, ~/.config/anthropic/api_key, ~/.anthropic_api_key,
               ~/.anthropic. Billed per token.
  claude-code  runs 'claude -p' on your Claude subscription (never an API key).
               Stops cleanly at --quota-stop of the 5-hour or 7-day window; resume later.
  openai       any OpenAI-compatible server at --base-url (default: a local server on 127.0.0.1:8000).
               --model is required; key optional (--openai-key-file, $OPENAI_API_KEY).`,
		Example: `  cull judge ~/Pictures/2026-09-26
  cull judge --backend claude-code -o cc.json ~/Pictures/2026-09-26
  cull judge --backend openai --model <model> ~/Pictures/2026-09-26
  cull judge --resume --write-xmp ~/Pictures/2026-09-26
  cull judge --move-culled ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if o.fresh && o.resume {
				return fmt.Errorf("--fresh and --resume contradict each other")
			}
			if o.xmpDevelop && !o.writeXMP {
				return fmt.Errorf("--xmp-develop requires --write-xmp")
			}
			if o.overwriteXMP && !o.writeXMP {
				return fmt.Errorf("--overwrite-xmp requires --write-xmp")
			}
			if _, err := o.policy.policy(); err != nil {
				return err
			}
			if o.concurrency < 0 {
				return fmt.Errorf("--concurrency must be >= 0 (0 = backend default)")
			}
			if err := o.backendFlags.validate(); err != nil {
				return err
			}
			if o.batch && o.backend != "anthropic" {
				return fmt.Errorf("--batch uses the Message Batches API: --backend anthropic only")
			}
			if o.batch && o.second {
				return fmt.Errorf("--second-opinion needs a synchronous run: drop --batch")
			}
			if o.batch && o.escalateBackend != "" {
				return fmt.Errorf("--batch can't be combined with --escalate-backend (escalate with a synchronous run afterwards)")
			}
			if o.escalateBackend != "" {
				if _, ok := backendDefaults[o.escalateBackend]; !ok {
					return fmt.Errorf("unknown --escalate-backend %q (want anthropic, claude-code, or openai)", o.escalateBackend)
				}
				if o.escalateModel == "" {
					return fmt.Errorf("--escalate-backend requires --escalate-model")
				}
				for _, s := range strings.Split(o.escalateOnList, ",") {
					if !escalateOn[strings.TrimSpace(s)] {
						return fmt.Errorf("--escalate-on %q: want a comma list of acceptable, soft, missed_focus, motion_blur, eyes_closed", s)
					}
				}
			}
			if o.maxCost < 0 {
				return fmt.Errorf("--max-cost must be >= 0")
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
			if o.model == "" {
				o.model = backendDefaults[o.backend].model
			}
			if err := checkPriced(cmd, o.backend, o.model, o.maxCost); err != nil {
				return err
			}
			if o.escalateBackend != "" {
				if err := checkPriced(cmd, o.escalateBackend, o.escalateModel, o.maxCost); err != nil {
					return fmt.Errorf("escalation: %w", err)
				}
			}
			price, priced := llm.PriceFor(o.backend, o.model)
			if o.estimate || priced {
				files, err := pipeline.Discover(cfg.Dir, cfg.Recursive)
				if err != nil {
					return err
				}
				usd := printEstimate(cmd, len(files), o.backend, o.model, price, priced, o.batch, !o.noRank, o.rankTwice)
				if o.second {
					fmt.Fprintln(cmd.ErrOrStderr(), "second opinions: one more evaluation per soft-or-worse frame, on top of the estimate")
				}
				if o.estimate {
					return nil
				}
				if err := confirmSpend(cmd, usd, o.yes); err != nil {
					return err
				}
			}
			b, auth, err := o.newBackend(cmd)
			if err != nil {
				return err
			}
			if priced {
				cfg.Price = &price
			}
			if cfg.Escalate, err = o.escalation(cmd); err != nil {
				return err
			}
			cfg.MaxCost = o.maxCost
			fmt.Fprintf(cmd.ErrOrStderr(), "backend: %s, model: %s, %s\n", b.Name(), o.model, auth)

			cfg.Backend = b.Name()
			cfg.Model = o.model
			cfg.Locate = o.locate == "model"
			cfg.Concurrency = o.backendFlags.concurrencyOrDefault(o.concurrency)
			cfg.Resume = o.resume
			cfg.Fresh = o.fresh
			cfg.WriteXMP = o.writeXMP
			cfg.XMPDevelop = o.xmpDevelop
			cfg.OverwriteXMP = o.overwriteXMP
			cfg.MoveCulled = o.moveCulled
			if o.moveCulled || o.writeXMP {
				if cfg.Labels, err = userLabels(cmd.ErrOrStderr(), cfg.ReportPath, o.labelsPath, o.noLabels); err != nil {
					return err
				}
			}
			cfg.RawClip = o.rawClip
			cfg.Policy, _ = o.policy.policy() // validated in PreRunE
			// Continuing a report: keep the policy its decisions came from.
			if o.resume {
				if prev, err := report.Load(cfg.ReportPath); err == nil {
					var notes []string
					if cfg.Policy, notes, err = o.policy.resolve(cmd.Flags(), prev.Policy); err != nil {
						return err
					}
					var seqNotes []string
					cfg.Seq, seqNotes = resolveSeq(cmd.Flags(), cfg.Seq, prev.Seq)
					if err := validSeq(cfg.Seq); err != nil {
						return fmt.Errorf("the report's stored grouping: %w", err)
					}
					noteStoredPolicy(cmd.ErrOrStderr(), append(notes, seqNotes...))
				}
			}
			cfg.CheckpointN = o.checkpoint
			cfg.Rank = !o.noRank
			cfg.SecondOpinion = o.second
			cfg.RankTwice = o.rankTwice
			cfg.Effort, cfg.LocateEffort = o.effort, o.locateEffort

			var rep *report.Report
			var usage eval.Usage
			out := so.out.newOutput(cmd, true)
			defer out.Close() // a panic must still release the terminal
			out.attach(&cfg)
			out.debugTo(b)
			if cfg.Escalate != nil {
				out.debugTo(cfg.Escalate.Backend)
			}
			if o.batch {
				cfg.Batch, cfg.BatchPoll = true, o.batchPoll
				rep, usage, err = pipeline.RunBatch(cmd.Context(), cfg, b.(*llm.Anthropic))
			} else {
				rep, usage, err = pipeline.Run(cmd.Context(), cfg, b)
			}
			out.Close()
			printSummary(cmd, cfg.ReportPath, rep, usage, b.Name(), o.batch)
			if errors.Is(err, llm.ErrBudget) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped at --max-cost; rerun with --resume (and a higher --max-cost) to continue")
			}
			if errors.Is(err, llm.ErrQuotaStop) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped early to protect your subscription quota; rerun later with --resume")
			}
			return err
		},
	}
	f := cmd.Flags()
	o.backendFlags.register(f)
	f.StringVar(&o.escalateBackend, "escalate-backend", "", "re-evaluate doubtful frames on a second backend (anthropic, claude-code, openai)")
	f.StringVar(&o.escalateModel, "escalate-model", "", "model for --escalate-backend (required with it)")
	f.StringVar(&o.escalateOnList, "escalate-on", "soft,missed_focus,motion_blur,eyes_closed", "first-pass outcomes that escalate")
	f.BoolVar(&o.rawClip, "raw-clip", true, "measure highlight clipping in the raw data (~0.8 s/frame); the preview overstates it")
	f.BoolVar(&o.batch, "batch", false, "use the Message Batches API (anthropic): half price, results within minutes to hours; Ctrl-C is safe, resume re-attaches")
	f.DurationVar(&o.batchPoll, "batch-poll", 30*time.Second, "how often --batch checks progress")
	f.BoolVar(&o.yes, "yes", false, "don't ask before spending (judge asks on a terminal when the estimate is over $1)")
	f.BoolVar(&o.estimate, "estimate", false, "print the cost estimate and exit (no model calls, no key needed)")
	f.Float64Var(&o.maxCost, "max-cost", 0, "stop once this run has cost this many USD at list price, or batch price with --batch (0 = no limit); resume later")
	f.StringVar(&o.locate, "locate", "model", "when no face is found, ask the model for the focus target: model or off")
	f.IntVarP(&o.concurrency, "concurrency", "j", 0, "parallel evaluations (0 = backend default: anthropic 4, claude-code 2, openai 4)")
	f.BoolVar(&o.resume, "resume", false, "skip files already evaluated in the existing report")
	f.BoolVar(&o.fresh, "fresh", false, "replace an existing report that holds assessments (default: refuse; see --resume)")
	f.BoolVar(&o.writeXMP, "write-xmp", false, "write XMP sidecars (rating, label, keyword)")
	f.BoolVar(&o.xmpDevelop, "xmp-develop", false, "also write Adobe crs exposure/crop (not applied by Capture One)")
	f.BoolVar(&o.overwriteXMP, "overwrite-xmp", false, "overwrite existing sidecars (default: never clobber)")
	f.StringVar(&o.labelsPath, "labels", "", "your labels log (default: cull-labels.jsonl beside the report, when it exists)")
	f.BoolVar(&o.noLabels, "no-labels", false, "ignore your labels (cull-labels.jsonl beside the report): moves and sidecar rewrites follow the model's verdicts")
	f.BoolVar(&o.moveCulled, "move-culled", false, "move frames decided cull (with their .xmp) into a culled/ folder beside them; undo with 'cull restore'. Use before importing into Capture One")
	f.BoolVar(&o.second, "second-opinion", false, "ask the model again about soft-or-worse frames (one more evaluation each, typically a minority of frames); when the two disagree, review")
	f.BoolVar(&o.rankTwice, "rank-twice", false, "rank each set of up to 8 frames a second time with its frames reversed; only places both orders agree on count (inside --keep-best in both: best; outside in both: outranked; else disputed, review). Doubles those calls")
	f.BoolVar(&o.noRank, "no-rank", false, "after judging, don't rank the sets that need it (run 'cull rank' separately later)")
	o.policy.register(f)
	f.IntVar(&o.checkpoint, "checkpoint", 25, "save the report every N results")
	cmd.MarkFlagFilename("api-key-file")
	return cmd
}

// escalateOn are the first-pass outcomes --escalate-on accepts.
var escalateOn = map[string]bool{"acceptable": true, "soft": true, "missed_focus": true, "motion_blur": true, "eyes_closed": true}

func (o *cullOpts) escalation(cmd *cobra.Command) (*pipeline.Escalation, error) {
	if o.escalateBackend == "" {
		return nil, nil
	}
	b, auth, err := o.buildBackend(cmd, o.escalateBackend, o.escalateModel)
	if err != nil {
		return nil, fmt.Errorf("escalation: %w", err)
	}
	e := &pipeline.Escalation{Backend: b, Model: o.escalateModel, On: map[string]bool{}}
	if p, ok := llm.PriceFor(o.escalateBackend, o.escalateModel); ok {
		e.Price = &p
	}
	for _, s := range strings.Split(o.escalateOnList, ",") {
		e.On[strings.TrimSpace(s)] = true
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "escalation: %s on %s, %s\n", e.Label(), o.escalateOnList, auth)
	return e, nil
}

// printEstimate prints and returns (the frames plus ranking) the list- or batch-price cost from measured per-frame token
// use, and, when rank is set (judge without --no-rank) and the backend is priced,
// a rough ranking cost: ⌈n/8⌉ calls, at 10k in / 1k out each. That's the call
// count if every frame lands in a full 8-frame set — neither a bound nor exact,
// since actual set sizes aren't known before judging: a pair still costs one
// call (more per frame than a full set), and a frame that joins no set costs
// nothing. It's a ballpark, not a gate.
func printEstimate(cmd *cobra.Command, n int, backend, model string, p llm.Price, priced, batch, rank, twice bool) float64 {
	w := cmd.ErrOrStderr()
	if !priced {
		fmt.Fprintf(w, "estimate: %d frames on %s (%s): no per-token cost (%s)\n", n, backend, model, backendDefaults[backend].basis)
		return 0
	}
	usd, in, out := llm.Estimate(n, p, batch)
	fmt.Fprintf(w, "estimate: %d frames × ~6k in / ~1k out tokens ≈ %d in / %d out ≈ $%.2f at %s (%s)\n", n, in, out, usd, rate(batch), model)
	if rank && n > 0 {
		calls := (n + 7) / 8
		if twice { // each 8-frame set is also ranked reversed
			calls *= 2
		}
		rusd, _, _ := llm.EstimateRank(calls, p, batch)
		fmt.Fprintf(w, "ranking ≈ %d call(s), $%.2f at %s, if every frame lands in an 8-frame set (pairs cost more per frame; frames in no set cost nothing)\n", calls, rusd, rate(batch))
		usd += rusd
	}
	return usd
}

// rate names the price basis that llm.Price.Cost applied.
func rate(batch bool) string {
	if batch {
		return "batch price (50%)"
	}
	return "list price"
}

func printSummary(cmd *cobra.Command, path string, rep *report.Report, usage eval.Usage, backend string, batch bool) {
	defer func() {
		if rep == nil {
			return
		}
		if cost := rep.Cost(); cost > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "cost in report: $%.2f at %s\n", cost, rate(batch))
		}
	}()
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
	if usage.TotalIn() > 0 {
		cached := ""
		if usage.CacheReadTokens+usage.CacheWriteTokens > 0 {
			cached = fmt.Sprintf(" (cache: %d written, %d read)", usage.CacheWriteTokens, usage.CacheReadTokens)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "tokens this run: in=%d%s out=%d (%s)\n", usage.TotalIn(), cached, usage.OutputTokens, backendDefaults[backend].basis)
	}
}
