package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

type rankOpts struct {
	backendFlags
	concurrency int
	estimate    bool
	maxCost     float64
	force       bool
	rankTwice   bool
	batch       bool
	batchPoll   time.Duration
	policy      policyFlags
}

func newRankCmd(so *sharedOpts) *cobra.Command {
	var o rankOpts
	cmd := &cobra.Command{
		Use:   "rank <dir>",
		Short: "Rank the sets judge found, without judging again",
		Long: `rank compares each set of similar frames (formed by judge from --seq-gap and
--seq-look) side by side with one model call per set (more for large sets, chunked
and merged into a final call, whether sent one wave at a time or, with --batch, all
at once). The --keep-best best of each set keep their decision; the rest get
--outranked. Run it after 'judge --no-rank'.

rank re-applies the full policy the way 'cull decide' does, starting from the
policy stored in the report (so tuning from an earlier decide carries over); a
policy flag given here overrides its own setting. --force re-ranks sets that already have a model order; if
only the policy changed (--keep-best, say), 'cull decide --keep-best N' re-applies
the stored order for free, so save --force for when the frames themselves changed
(a re-judge, added frames).

Unless --backend/--model are given, they default to the values the report was
judged with (--batch always uses anthropic). It only updates the report; write
sidecars or move culls with a following 'cull decide --write-xmp --move-culled'.`,
		Example: `  cull rank ~/Pictures/2026-09-26
  cull rank --backend claude-code ~/Pictures/2026-09-26
  cull rank --force ~/Pictures/2026-09-26
  cull rank --batch ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.policy.policy(); err != nil {
				return err
			}
			if err := o.backendFlags.validate(); err != nil {
				return err
			}
			if o.concurrency < 0 {
				return fmt.Errorf("--concurrency must be >= 0 (0 = backend default)")
			}
			if o.maxCost < 0 {
				return fmt.Errorf("--max-cost must be >= 0")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			// Sync cull rank refuses up front while a rank-batch state is
			// recorded, before RankCalls/fillLooks run for nothing (that's a
			// full DNG decode pass on a schema-v3 report) for a run that's
			// about to refuse anyway. --batch itself is exempt: re-attaching to
			// that state is the whole point.
			if !o.batch {
				if err := pipeline.RankBatchPending(cfg); err != nil {
					return err
				}
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return err
			}
			var notes []string
			if cfg.Policy, notes, err = o.policy.resolve(cmd.Flags(), rep.Policy); err != nil {
				return err
			}
			var seqNotes []string
			cfg.Seq, seqNotes = resolveSeq(cmd.Flags(), cfg.Seq, rep.Seq)
			if err := validSeq(cfg.Seq); err != nil {
				return fmt.Errorf("the report's stored grouping: %w", err)
			}
			noteStoredPolicy(cmd.ErrOrStderr(), append(notes, seqNotes...))

			warning, err := o.applyBackendModel(&cfg, cmd.Flags().Changed("backend"), cmd.Flags().Changed("model"), rep)
			if err != nil {
				return err
			}
			if warning != "" {
				warn(cmd, []string{warning})
			}

			if err := checkPriced(cmd, o.backend, o.model, o.maxCost); err != nil {
				return err
			}
			cfg.RankTwice = o.rankTwice
			cfg.Effort = o.effort
			price, priced := llm.PriceFor(o.backend, o.model)
			if o.estimate || priced {
				sets, calls, filled, cerr := pipeline.RankCalls(cmd.Context(), rep, cfg, o.force, cmd.ErrOrStderr())
				if filled > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "computed the look of %d frame(s) from their DNGs\n", filled)
					if serr := rep.Save(cfg.ReportPath); serr != nil {
						return serr
					}
				}
				if cerr != nil {
					return cerr
				}
				printRankEstimate(cmd, sets, calls, o.backend, o.model, price, priced, o.batch)
				if o.estimate {
					return nil
				}
			}
			b, auth, err := o.newBackend(cmd)
			if err != nil {
				return err
			}
			if priced {
				cfg.Price = &price
			}
			cfg.MaxCost = o.maxCost
			cfg.Concurrency = o.backendFlags.concurrencyOrDefault(o.concurrency)
			fmt.Fprintf(cmd.ErrOrStderr(), "backend: %s, model: %s, %s\n", b.Name(), o.model, auth)
			cfg.Log = cmd.ErrOrStderr()

			var out *report.Report
			if o.batch {
				cfg.Batch, cfg.BatchPoll = true, o.batchPoll
				out, err = pipeline.RankBatch(cmd.Context(), cfg, b.(*llm.Anthropic), o.force)
			} else {
				out, err = pipeline.Rank(cmd.Context(), cfg, b, o.force)
			}
			printSummary(cmd, cfg.ReportPath, out, eval.Usage{}, b.Name(), o.batch)
			if errors.Is(err, llm.ErrBudget) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped at --max-cost; rerun (and raise --max-cost) to rank the rest")
			}
			if errors.Is(err, llm.ErrQuotaStop) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped early to protect your subscription quota; rerun later to continue ranking")
			}
			return err
		},
	}
	f := cmd.Flags()
	o.backendFlags.register(f)
	f.IntVarP(&o.concurrency, "concurrency", "j", 0, "parallel rank calls (0 = backend default: anthropic 4, claude-code 2, openai 4)")
	f.BoolVar(&o.estimate, "estimate", false, "print the cost estimate and exit (no model calls, no key needed)")
	f.Float64Var(&o.maxCost, "max-cost", 0, "stop once this run has cost this many USD at list price, or batch price with --batch (0 = no limit)")
	f.BoolVar(&o.rankTwice, "rank-twice", false, "rank each set of up to 8 frames a second time with its frames reversed; only places both orders agree on count (inside --keep-best in both: best; outside in both: outranked; else disputed, review). Doubles those calls")
	f.BoolVar(&o.force, "force", false, "re-rank every set of two or more rankable frames, even one that already has a model order")
	f.BoolVar(&o.batch, "batch", false, "use the Message Batches API (anthropic): half price, results within minutes to hours; Ctrl-C is safe, rerun re-attaches")
	f.DurationVar(&o.batchPoll, "batch-poll", 30*time.Second, "how often --batch checks progress")
	o.policy.register(f)
	cmd.MarkFlagFilename("api-key-file")
	return cmd
}

// applyBackendModel resolves rank's --backend/--model: unless given explicitly
// (backendChanged, modelChanged — from cmd.Flags().Changed), they default to
// the values rep was judged with; the model only defaults from the report when
// the backend also matches it (the report's model name means nothing paired
// with a different, explicitly-chosen backend). It then applies --batch's
// anthropic-only override: an explicit non-anthropic --backend fails, with no
// warning printed (the caller should print none before returning this error);
// otherwise --backend is forced to anthropic, overriding whatever the
// report-based default above picked, and the model defaults from the report
// only when the report's own backend is anthropic too — otherwise it's
// anthropic's own default model, not the other backend's.
//
// It sets cfg.Backend/cfg.Model to the backend/model rank ends up using (as
// judge sets them too — cull.go), so a rank-batch state rank writes or reads
// records and checks against the right values.
//
// The report-mismatch warning to print is computed and returned last, AFTER
// the --batch override: printed before it, a claude-code-judged report's
// warning would say "ranking with claude-code/sonnet" right before silently
// moving to anthropic, and an explicit non-anthropic --backend would print a
// warning for a run that's about to fail anyway.
func (o *rankOpts) applyBackendModel(cfg *pipeline.Config, backendChanged, modelChanged bool, rep *report.Report) (warning string, err error) {
	if !backendChanged && rep.Backend != "" {
		o.backend = rep.Backend
	}
	if !modelChanged && rep.Model != "" && o.backend == rep.Backend {
		o.model = rep.Model
	}
	if o.model == "" {
		o.model = backendDefaults[o.backend].model
	}
	if o.batch {
		if backendChanged {
			if o.backend != "anthropic" {
				return "", fmt.Errorf("--batch uses the Message Batches API: --backend anthropic only")
			}
		} else {
			o.backend = "anthropic"
			if !modelChanged {
				if rep.Backend == "anthropic" && rep.Model != "" {
					o.model = rep.Model
				} else {
					o.model = backendDefaults["anthropic"].model
				}
			}
		}
	}
	if rep.Backend != "" && (rep.Backend != o.backend || rep.Model != o.model) {
		warning = fmt.Sprintf("report was judged with %s/%s; ranking with %s/%s", rep.Backend, rep.Model, o.backend, o.model)
	}
	cfg.Backend, cfg.Model = o.backend, o.model
	return warning, nil
}

// printRankEstimate projects list- or batch-price ranking cost from an exact
// sets/calls count (pipeline.RankCalls), computed without calling any model.
func printRankEstimate(cmd *cobra.Command, sets, calls int, backend, model string, p llm.Price, priced, batch bool) {
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "estimate: %d set%s, %d call%s to rank", sets, plural(sets), calls, plural(calls))
	if !priced {
		fmt.Fprintf(w, ": no per-token cost (%s)\n", backendDefaults[backend].basis)
		return
	}
	usd, in, out := llm.EstimateRank(calls, p, batch)
	fmt.Fprintf(w, " ≈ %d in / %d out ≈ $%.2f at %s (%s)\n", in, out, usd, rate(batch), model)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
