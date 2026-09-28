package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/calib"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func newCalibrateCmd() *cobra.Command {
	var (
		labelsPath string
		pol        policyFlags
	)
	cmd := &cobra.Command{
		Use:   "calibrate [--labels gophotocull-labels.jsonl] REPORT.json...",
		Short: "Measure how well reports agree with your hand labels",
		Long: `calibrate compares each report's decisions with your labels from the review
sheet (gophotocull-labels.jsonl beside the first report, unless --labels): a
confusion matrix, the false-cull rate (you said keep, it culled), the missed-cull
rate, and the review rate. Star ratings without a label don't count. It also
re-decides the stored assessments across --review-below-sharpness values, so
thresholds can be tuned without new model calls; apply the chosen ones with
'gophotocull decide'.`,
		Example: "  gophotocull calibrate sonnet.json local.json",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := pol.policy()
			if err != nil {
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
				return fmt.Errorf("no labels in %s: label frames with 'gophotocull review --serve' first, or pass --labels", labelsPath)
			}
			w := cmd.OutOrStdout()
			for _, path := range args {
				rep, err := report.Load(path)
				if err != nil {
					return err
				}
				calib.Format(w, path, rep, calib.Compare(rep, verdicts))
				calib.FormatSweep(w, calib.Sweep(rep, verdicts, p, []float64{0, 3, 4, 5, 6, 7, 8}))
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "labels log (default: gophotocull-labels.jsonl beside the first report)")
	pol.register(cmd.Flags())
	return cmd
}
