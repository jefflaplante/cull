package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
)

func newImportLabelsCmd(so *sharedOpts) *cobra.Command {
	var labelsPath string
	cmd := &cobra.Command{
		Use:   "import-labels <export.jsonl> <dir>",
		Short: "Add labels exported from a 'review --static' page to the shoot's labels log",
		Long: `A static review page (review --static) keeps labels in the browser; its "Export
labels" button saves them as JSONL in the labels log's own format. import-labels
appends them to the shoot's log (cull-labels.jsonl beside the report, or --labels),
where calibrate, decide and apply-c1 read them. Every label must name a frame in the
report: one that doesn't came from another shoot's page, and nothing is imported.`,
		Example: "  cull import-labels ~/Downloads/cull-labels.jsonl ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[1])
			if err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return fmt.Errorf("no report: %w (run scan or judge first, or pass -o)", err)
			}
			inReport := map[string]bool{}
			for _, r := range rep.Results {
				inReport[filepath.Base(r.File)] = true
			}
			in, err := labels.Read(args[0]) // the export is the log's format: the last line per frame wins
			if err != nil {
				return err
			}
			var names, strangers []string
			for f := range in {
				if inReport[f] {
					names = append(names, f)
				} else {
					strangers = append(strangers, f)
				}
			}
			if len(strangers) > 0 {
				sort.Strings(strangers)
				return fmt.Errorf("not in the report (another shoot's page?): %s; nothing imported", strings.Join(strangers, ", "))
			}
			sort.Strings(names)
			log := labelsOr(labelsPath, cfg.ReportPath)
			for _, f := range names {
				if err := labels.Append(log, in[f]); err != nil {
					return fmt.Errorf("after importing some labels: %w", err)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d label(s) into %s\n", len(names), log)
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "the labels log to append to (default: cull-labels.jsonl beside the report)")
	return cmd
}
