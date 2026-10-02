package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/pipeline"
)

func newRestoreCmd(so *sharedOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <dir>",
		Short: "Move frames that --move-culled or --sort moved back to where they were",
		Long: `restore reads the report and moves every frame recorded as moved into a
culled/ folder (and its .xmp sidecar) back to its original path, then removes
culled/ folders left empty. It never overwrites: a frame whose original path is
taken again stays in culled/ and is listed. To keep some culls, move those
files back by hand instead.`,
		Example: "  cull restore ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			n, err := pipeline.Restore(cfg.ReportPath, cfg.Dir, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "restored %d frame(s)\n", n)
			return nil
		},
	}
}
