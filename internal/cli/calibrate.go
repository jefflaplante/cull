package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/calib"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

func newCalibrateCmd() *cobra.Command {
	var (
		labelsPath string
		pol        policyFlags
	)
	cmd := &cobra.Command{
		Use:   "calibrate [--labels cull-labels.jsonl] REPORT.json...",
		Short: "Measure how well reports agree with your hand labels",
		Long: `calibrate compares each report's decisions with your labels from the review
sheet (cull-labels.jsonl beside the first report, unless --labels): a
confusion matrix, the false-cull rate (you said keep, it culled), the missed-cull
rate, and the review rate. Star ratings without a label don't count. It also
re-decides the stored assessments across --review-below-sharpness values, so
thresholds can be tuned without new model calls; apply the chosen ones with
'cull decide'. Reports with multi-frame sets get a sets section: how often a
labeled keep was ranked out of the keep-best cut, how often a labeled cull or
review was ranked into it, and a keep-best 1..5 sweep from the stored ranks.`,
		Example: "  cull calibrate sonnet.json local.json",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := pol.policy(); err != nil {
				return err
			}
			if labelsPath == "" {
				labelsPath = labels.DefaultPath(args[0])
			}
			all, err := labels.Read(labelsPath)
			if err != nil {
				return err
			}
			verdicts := labels.Verdicts(all)
			if len(verdicts) == 0 {
				return fmt.Errorf("no labels in %s: label frames with 'cull review' first, or pass --labels", labelsPath)
			}
			w := cmd.OutOrStdout()
			for _, path := range args {
				rep, err := report.Load(path)
				if err != nil {
					return err
				}
				if dups := labels.Duplicates(rep.Results); len(dups) > 0 {
					return fmt.Errorf("%s: frames share a file name, so your labels can't tell them apart (rename them): %s", path, strings.Join(dups, "; "))
				}
				// Each report's sweep starts from the policy and grouping its decisions came from.
				p, notes, err := pol.resolve(cmd.Flags(), rep.Policy)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				seq, seqNotes := resolveSeq(cmd.Flags(), flagSeq(cmd), rep.Seq)
				if err := validSeq(seq); err != nil {
					return fmt.Errorf("%s: the report's stored grouping: %w", path, err)
				}
				noteStoredPolicy(cmd.ErrOrStderr(), append(notes, seqNotes...))
				redecide := func(p eval.Policy) *report.Report { return pipeline.DecideCopy(rep, p, seq) }
				calib.Format(w, path, rep, calib.Compare(rep, verdicts))
				calib.FormatSweep(w, calib.Sweep(rep, verdicts, p, []float64{0, 3, 4, 5, 6, 7, 8}, redecide))
				calib.FormatSets(w, calib.Sets(rep, verdicts, p.KeepBest), calib.SweepKeepBest(rep, verdicts, []int{1, 2, 3, 4, 5}))
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "labels log (default: cull-labels.jsonl beside the first report)")
	pol.register(cmd.Flags())
	return cmd
}

// flagSeq is the grouping the --seq-gap / --seq-look flags hold (typed or default);
// calibrate takes a report path, not a shoot folder, so it doesn't build a Config.
func flagSeq(cmd *cobra.Command) group.Options {
	gap, _ := cmd.Flags().GetDuration("seq-gap")
	look, _ := cmd.Flags().GetFloat64("seq-look")
	return group.Options{Gap: gap, MaxLook: look}
}
