package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/pipeline"
	"github.com/jefflaplante/gophotocull/internal/report"
)

type rankOpts struct {
	backendFlags
	estimate bool
	maxCost  float64
	force    bool
	policy   policyFlags
}

func newRankCmd(so *sharedOpts) *cobra.Command {
	var o rankOpts
	cmd := &cobra.Command{
		Use:   "rank <dir>",
		Short: "Rank the sets judge found, without judging again",
		Long: `rank compares each set of similar frames (formed by judge from --seq-gap and
--seq-look) side by side with one model call per set (more for large sets, chunked
and merged into a final call). The --keep-best best of each set keep their decision;
the rest get --outranked. Run it after 'judge --no-rank', or again with --force to
re-rank sets that already have a model order (e.g. after changing --keep-best).
It only updates the report; write sidecars or move culls with a following 'cull
decide --write-xmp --move-culled'.`,
		Example: `  cull rank ~/Pictures/2026-09-26
  cull rank --backend claude-code ~/Pictures/2026-09-26
  cull rank --force --keep-best 1 ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.policy.policy(); err != nil {
				return err
			}
			if err := o.backendFlags.validate(); err != nil {
				return err
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
			if cfg.Policy, err = o.policy.policy(); err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return err
			}
			if o.model == "" {
				o.model = backendDefaults[o.backend].model
			}
			price, priced := llm.PriceFor(o.backend, o.model)
			if o.estimate || priced {
				printRankEstimate(cmd, setsNeedingRanking(rep), pipeline.RankCalls(rep, cfg), o.backend, o.model, price, priced)
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
			if rep.Backend != "" && (rep.Backend != b.Name() || rep.Model != o.model) {
				warn(cmd, []string{fmt.Sprintf("report was judged with %s/%s; ranking with %s/%s", rep.Backend, rep.Model, b.Name(), o.model)})
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "backend: %s, model: %s, %s\n", b.Name(), o.model, auth)
			cfg.Log = cmd.ErrOrStderr()

			out, err := pipeline.Rank(cmd.Context(), cfg, b, o.force)
			printSummary(cmd, cfg.ReportPath, out, eval.Usage{}, b.Name(), false)
			if errors.Is(err, llm.ErrBudget) {
				fmt.Fprintln(cmd.ErrOrStderr(), "stopped at --max-cost; rerun (and raise --max-cost) to rank the rest")
			}
			return err
		},
	}
	f := cmd.Flags()
	o.backendFlags.register(f)
	f.BoolVar(&o.estimate, "estimate", false, "print the cost estimate and exit (no model calls, no key needed)")
	f.Float64Var(&o.maxCost, "max-cost", 0, "stop once this run has cost this many USD at list price (0 = no limit)")
	f.BoolVar(&o.force, "force", false, "re-rank every set of two or more rankable frames, even one that already has a model order")
	o.policy.register(f)
	cmd.MarkFlagFilename("api-key-file")
	return cmd
}

// setsNeedingRanking mirrors pipeline's needsRanking predicate (unexported there):
// a set of at least two rankable frames with no stored model order covering them.
func setsNeedingRanking(rep *report.Report) int {
	n := 0
	for _, s := range rep.Sets {
		if s.Of >= 2 && s.By != "model" {
			n++
		}
	}
	return n
}

// printRankEstimate projects list-price ranking cost from sets and calls, computed
// without loading any image or calling any model.
func printRankEstimate(cmd *cobra.Command, sets, calls int, backend, model string, p llm.Price, priced bool) {
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "estimate: %d set%s, %d call%s needing ranking", sets, plural(sets), calls, plural(calls))
	if !priced {
		fmt.Fprintf(w, ": no per-token cost (%s)\n", backendDefaults[backend].basis)
		return
	}
	usd, in, out := llm.EstimateRank(calls, p, false)
	fmt.Fprintf(w, " ≈ %d in / %d out ≈ $%.2f at %s (%s)\n", in, out, usd, rate(false), model)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
