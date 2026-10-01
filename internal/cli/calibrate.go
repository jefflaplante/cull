package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		compare    bool
	)
	cmd := &cobra.Command{
		Use:   "calibrate [--labels cull-labels.jsonl] <dir|REPORT.json>...",
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
		Example: `  cull calibrate sonnet.json local.json
  cull calibrate --compare run1.json run2.json   # run-to-run stability; no labels needed`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for i, a := range args {
				args[i] = reportArg(a)
			}
			if compare {
				// Two runs over the same frames: how often they disagree, which
				// bounds how far any single run's verdicts can be trusted.
				if len(args) != 2 {
					return fmt.Errorf("--compare takes exactly two reports, got %d", len(args))
				}
				a, err := report.Load(args[0])
				if err != nil {
					return err
				}
				b, err := report.Load(args[1])
				if err != nil {
					return err
				}
				d, err := calib.CompareRuns(a, b)
				if err != nil {
					return err
				}
				calib.FormatRunDiff(cmd.OutOrStdout(), args[0], args[1], d)
				return nil
			}
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
	var seqGap time.Duration
	var seqLook float64
	registerSeqVars(cmd.Flags(), &seqGap, &seqLook) // flagSeq reads them
	cmd.Flags().BoolVar(&compare, "compare", false, "compare two reports over the same frames (two runs, or two backends): how often their verdicts disagree; no labels needed")
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

// reportArg is the report a calibrate argument names: a shoot folder means the
// cull-report.json inside it, so the labels log is found beside it too.
func reportArg(p string) string {
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return filepath.Join(p, "cull-report.json")
	}
	return p
}
