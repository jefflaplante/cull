package cli

import (
	"github.com/spf13/cobra"
)

func newScanCmd(so *sharedOpts) *cobra.Command {
	var concurrency int
	cmd := &cobra.Command{
		Use:   "scan <dir>",
		Short: "Extract and measure previews without calling the API",
		Long: `scan extracts each DNG's embedded preview and records its resolution, source,
exposure statistics, and peak sharpness. No API key is needed and nothing is written
next to your images; only the report is produced.

Use it to confirm the previews are large enough for focus judgement.`,
		Example: "  gophotocull scan ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			cfg.DryRun = true
			cfg.Concurrency = concurrency
			rep, usage, err := runPipeline(cmd, cfg, nil)
			printSummary(cmd, cfg.ReportPath, rep, usage)
			return err
		},
	}
	cmd.Flags().IntVarP(&concurrency, "concurrency", "j", 8, "parallel extractions")
	return cmd
}
