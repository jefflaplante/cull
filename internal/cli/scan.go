package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newScanCmd(so *sharedOpts) *cobra.Command {
	var concurrency int
	var rawClip bool
	cmd := &cobra.Command{
		Use:   "scan <dir>",
		Short: "Extract and measure previews without calling the API",
		Long: `scan extracts each DNG's embedded preview and records its resolution, source,
exposure statistics, face detection, and the "where focus landed" measure. No model
is called and nothing is written next to your images; only the report is produced.

Use it to confirm the previews are large enough for focus judgement, and with
--save-inputs to see exactly what the model would be shown.`,
		Example: "  gophotocull scan ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			cfg.DryRun = true
			cfg.Concurrency = concurrency
			cfg.RawClip = rawClip
			rep, usage, err := runPipeline(cmd, cfg, nil)
			printSummary(cmd, cfg.ReportPath, rep, usage, "")
			if rep != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), ScanSummary(rep))
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&rawClip, "raw-clip", false, "also measure highlight clipping in the raw data (~0.8 s/frame)")
	cmd.Flags().IntVarP(&concurrency, "concurrency", "j", 4, "parallel extractions (~1 GB RAM each for 60MP previews)")
	return cmd
}
