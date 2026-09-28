package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/calib"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func newCalibrateCmd() *cobra.Command {
	var (
		labelsPath string
		pol        policyFlags
	)
	cmd := &cobra.Command{
		Use:   "calibrate --labels labels.csv REPORT.json...",
		Short: "Measure how well reports agree with your hand labels",
		Long: `calibrate compares each report's decisions with labels.csv ("file,label", as
exported by the review sheet): a confusion matrix, the false-cull rate (you said
keep, it culled), the missed-cull rate, and the review rate. It also re-decides the
stored assessments across --review-below-sharpness values, so thresholds can be
tuned without new model calls; apply the chosen ones with 'gophotocull decide'.`,
		Example: "  gophotocull calibrate --labels labels.csv sonnet.json local.json",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if labelsPath == "" {
				return fmt.Errorf("--labels is required")
			}
			p, err := pol.policy()
			if err != nil {
				return err
			}
			f, err := os.Open(labelsPath)
			if err != nil {
				return err
			}
			labels, err := calib.ReadLabels(f)
			f.Close()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, path := range args {
				rep, err := report.Load(path)
				if err != nil {
					return err
				}
				calib.Format(w, path, rep, calib.Compare(rep, labels))
				calib.FormatSweep(w, calib.Sweep(rep, labels, p, []float64{0, 3, 4, 5, 6, 7, 8}))
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "labels CSV (file,label) from the review sheet")
	pol.register(cmd.Flags())
	return cmd
}
