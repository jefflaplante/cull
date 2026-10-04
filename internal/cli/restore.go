package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/ui"
)

func newRestoreCmd(so *sharedOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <dir>",
		Short: "Move frames that --move-culled or --sort moved back to where they were",
		Long: `restore reads the report and moves every frame recorded as moved (into culled/
by --move-culled, or into keep/, review/ or cull/ by --sort), with its .xmp sidecar,
back to its original path, then removes those folders where they are left empty. It
never overwrites: a frame whose original path is taken again stays where it is and
is listed. To keep some frames where they are, move the others back by hand instead.`,
		Example: "  cull restore ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			out := so.out.newOutput(cmd, true)
			n, err := pipeline.Restore(cfg.ReportPath, cfg.Dir, ui.LineWriter(out.UI, ui.Quiet, ui.Warn), out.UI)
			out.Close()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "restored %d frame(s)\n", n)
			return nil
		},
	}
}
