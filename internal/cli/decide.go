package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/pipeline"
)

func newDecideCmd(so *sharedOpts) *cobra.Command {
	var (
		pol                                    policyFlags
		writeXMP, xmpDevelop, overwrite, moveC bool
		labelsPath                             string
		noLabels                               bool
	)
	cmd := &cobra.Command{
		Use:   "decide <dir>",
		Short: "Re-apply the keep/review/cull policy to a report without calling a model",
		Long: `decide re-runs the policy on every assessment stored in the report, so tuning
thresholds after calibration is free and instant. It prints what changed and saves
the report. --write-xmp rewrites sidecars the report says gophotocull wrote (and
creates missing ones); other sidecars are never touched unless --overwrite-xmp.
--move-culled syncs culled/: new culls move there, frames no longer culled come back.
Your labels from the review sheet (gophotocull-labels.jsonl beside the report, or
--labels) are used where you gave one: your verdicts drive sidecars and moves (the
report keeps the model's), your stars become sidecar ratings. --no-labels ignores them.`,
		Example: `  gophotocull decide --review-below-sharpness 6 --eyes-closed cull ~/Pictures/2026-09-26
  gophotocull decide --write-xmp --move-culled ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			p, err := pol.policy()
			if err != nil {
				return err
			}
			if xmpDevelop && !writeXMP {
				return fmt.Errorf("--xmp-develop requires --write-xmp")
			}
			lab, err := userLabels(cmd.ErrOrStderr(), cfg.ReportPath, labelsPath, noLabels)
			if err != nil {
				return err
			}
			sum, err := pipeline.Decide(cfg.ReportPath, pipeline.DecideOptions{
				Policy: p, WriteXMP: writeXMP, XMPDevelop: xmpDevelop, OverwriteXMP: overwrite, MoveCulled: moveC,
				GroupGap: cfg.GroupGap, GroupHamming: cfg.GroupHamming, Labels: lab,
			}, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), describeDecide(sum))
			return nil
		},
	}
	f := cmd.Flags()
	pol.register(f)
	f.BoolVar(&writeXMP, "write-xmp", false, "rewrite our sidecars (and create missing ones) for the new decisions")
	f.BoolVar(&xmpDevelop, "xmp-develop", false, "also write Adobe crs exposure/crop (not applied by Capture One)")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "also overwrite sidecars gophotocull did not write")
	f.BoolVar(&moveC, "move-culled", false, "sync culled/: move new culls there, restore frames no longer culled")
	f.StringVar(&labelsPath, "labels", "", "your labels log (default: gophotocull-labels.jsonl beside the report, when it exists)")
	f.BoolVar(&noLabels, "no-labels", false, "ignore your labels: sidecars and moves follow the model's verdicts")
	return cmd
}

func describeDecide(s pipeline.DecideSummary) string {
	var changes []string
	for k, n := range s.Changed {
		changes = append(changes, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(changes)
	msg := fmt.Sprintf("decided %d frame(s); ", s.Frames)
	if len(changes) == 0 {
		msg += "no decision changed"
	} else {
		msg += "changed: " + strings.Join(changes, ", ")
	}
	if s.Moved+s.Restored > 0 {
		msg += fmt.Sprintf("; moved %d into culled/, restored %d", s.Moved, s.Restored)
	}
	return msg
}
