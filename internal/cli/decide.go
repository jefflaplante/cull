package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

func newDecideCmd(so *sharedOpts) *cobra.Command {
	var (
		pol                                    policyFlags
		writeXMP, xmpDevelop, overwrite, moveC bool
		sortF                                  sortMode
		labelsPath                             string
		noLabels                               bool
	)
	cmd := &cobra.Command{
		Use:   "decide <dir>",
		Short: "Re-apply the keep/review/cull policy to a report without calling a model",
		Long: `decide re-runs the policy on every assessment stored in the report, so tuning
thresholds after calibration is free and instant. It prints what changed and saves
the report, with the policy it used: later runs (decide, rank, calibrate, judge
--resume) start from that stored policy, and only the flags you give override it. --write-xmp rewrites sidecars the report says cull wrote (and
creates missing ones); other sidecars are never touched unless --overwrite-xmp.
--sort syncs keep/, review/ and cull/ with the verdicts; --sort=culls only cull/: new
culls move there, frames no longer culled come back.
Your labels from the review sheet (cull-labels.jsonl beside the report, or
--labels) are used where you gave one: your verdicts drive sidecars and moves (the
report keeps the model's), your stars become sidecar ratings. --no-labels ignores them.`,
		Example: `  cull decide --review-below-sharpness 6 --eyes-closed cull ~/Pictures/2026-09-26
  cull decide --write-xmp --sort=culls ~/Pictures/2026-09-26`,
		Args: sortArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if moveC {
				if sortF == sortAll {
					return fmt.Errorf("--sort and --move-culled can't be combined: --sort already puts culls in cull/")
				}
				sortF = sortCulls
			}
			moveCulls, sortAllF := sortF.flags()
			if overwrite && !writeXMP {
				return fmt.Errorf("--overwrite-xmp requires --write-xmp")
			}
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			var saved *eval.Policy
			var savedSeq *report.Sequences
			if rep, err := report.Load(cfg.ReportPath); err == nil { // a missing report is Decide's error to report
				saved, savedSeq = rep.Policy, rep.Seq
				if rep.Backend == "" { // a scan: there are no verdicts to decide or sort
					return fmt.Errorf("%s is a scan report with no judged frames: run cull judge %s first", cfg.ReportPath, args[0])
				}
			}
			p, notes, err := pol.resolve(cmd.Flags(), saved)
			if err != nil {
				return err
			}
			var seqNotes []string
			cfg.Seq, seqNotes = resolveSeq(cmd.Flags(), cfg.Seq, savedSeq)
			if err := validSeq(cfg.Seq); err != nil {
				return fmt.Errorf("the report's stored grouping: %w", err)
			}
			noteStoredPolicy(cmd.ErrOrStderr(), append(notes, seqNotes...))
			if xmpDevelop && !writeXMP {
				return fmt.Errorf("--xmp-develop requires --write-xmp")
			}
			lab, err := userLabels(cmd.ErrOrStderr(), cfg.ReportPath, labelsPath, noLabels)
			if err != nil {
				return err
			}
			out := so.out.newOutput(cmd, true) // looks, sidecars and moves can take a while
			sum, err := pipeline.Decide(cmd.Context(), cfg.ReportPath, pipeline.DecideOptions{
				Dir: cfg.Dir, Policy: p, WriteXMP: writeXMP, XMPDevelop: xmpDevelop, OverwriteXMP: overwrite, MoveCulled: moveCulls, Sort: sortAllF,
				Seq: cfg.Seq, Labels: lab, UI: out.UI,
			}, out.Log)
			out.Close()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), describeDecide(sum, sortAllF))
			return nil
		},
	}
	f := cmd.Flags()
	pol.register(f)
	f.BoolVar(&writeXMP, "write-xmp", false, "rewrite our sidecars (and create missing ones) for the new decisions")
	f.BoolVar(&xmpDevelop, "xmp-develop", false, "also write Adobe crs exposure/crop (not applied by Capture One)")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "also overwrite sidecars not written by cull")
	f.BoolVar(&moveC, "move-culled", false, "deprecated: use --sort=culls")
	registerSort(f, &sortF, "sync the folders with the current verdicts (your labels first): --sort or --sort=all keep/, review/, cull/; --sort=culls only cull/. Undo with 'cull restore'")
	f.StringVar(&labelsPath, "labels", "", "your labels log (default: cull-labels.jsonl beside the report, when it exists)")
	f.BoolVar(&noLabels, "no-labels", false, "ignore your labels: sidecars and moves follow the model's verdicts")
	setSection(f, secSidecars, "write-xmp", "xmp-develop", "overwrite-xmp", "labels", "no-labels", "move-culled")
	f.MarkDeprecated("move-culled", "use --sort=culls (culls now go into cull/)")
	return cmd
}

func describeDecide(s pipeline.DecideSummary, sorted bool) string {
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
	switch {
	case s.Moved+s.Restored == 0:
	case sorted:
		msg += fmt.Sprintf("; sorted %d into keep/, review/ and cull/, %d back into the shoot folder", s.Moved, s.Restored)
	default:
		msg += fmt.Sprintf("; moved %d into cull/, %d back into the shoot folder", s.Moved, s.Restored)
	}
	return msg
}
