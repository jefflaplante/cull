package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/review"
)

func newReviewCmd(so *sharedOpts) *cobra.Command {
	var (
		out   string
		jobs  int
		force bool
	)
	cmd := &cobra.Command{
		Use:   "review <dir>",
		Short: "Write an HTML contact sheet for checking decisions and labeling frames",
		Long: `review writes a self-contained HTML page (no network) from the report: every frame
with the subject crop the model judged, the decision and its reasons. Label frames
keep/review/cull with the keyboard; labels are saved in the browser and exported
as labels.csv for 'gophotocull calibrate'. Works on scan reports too (labeling only).`,
		Example: "  gophotocull review ~/Pictures/2026-09-26 && open ~/Pictures/2026-09-26/gophotocull-review/index.html",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return fmt.Errorf("no report: %w (run scan or cull first, or pass -o)", err)
			}
			if out == "" {
				out = filepath.Join(filepath.Dir(cfg.ReportPath), "gophotocull-review")
			}
			index, err := review.Build(rep, cfg.ReportPath, review.Options{Out: out, Concurrency: jobs, Force: force}, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "review sheet: %s\nopen it with: open %q\n", index, index)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output directory (default: gophotocull-review next to the report)")
	cmd.Flags().IntVarP(&jobs, "concurrency", "j", 4, "parallel image rendering (~200 MB RAM each)")
	cmd.Flags().BoolVar(&force, "force", false, "re-render images that already exist")
	return cmd
}
