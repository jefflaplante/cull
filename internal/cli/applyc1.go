package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/c1"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
)

func newApplyC1Cmd(so *sharedOpts) *cobra.Command {
	var (
		o          c1.Options
		probe      bool
		runIt      bool
		osascript  string
		labelsPath string
		noLabels   bool
	)
	cmd := &cobra.Command{
		Use:   "apply-c1 [--probe | <dir>]",
		Short: "Generate (and optionally run) AppleScript that applies the report in Capture One",
		Long: `apply-c1 writes an AppleScript for the open Capture One document: a color tag
(keep green, review yellow, cull red) and a cull:<verdict> keyword per frame,
and with --exposure / --crop the suggested exposure and crop. Your labels from the
review sheet (cull-labels.jsonl beside the report, or --labels; --no-labels
ignores them) override the model's verdicts, and your stars set ratings; ratings
are never set otherwise, so ratings made in Capture One stay. It prints the script by default (a dry run); --run pipes it to
osascript.

Run --probe first: a read-only script that lists a few images with their names,
color tags, dimensions and crops, to confirm how Capture One names and numbers
things before anything is written.`,
		Example: `  cull apply-c1 --probe ~/Pictures/2026-09-26 | osascript -
  cull apply-c1 ~/Pictures/2026-09-26 > apply.applescript   # review it
  cull apply-c1 --run ~/Pictures/2026-09-26`,
		Args: cobra.RangeArgs(0, 1), // --probe reads the open Capture One document, not a folder
		RunE: func(cmd *cobra.Command, args []string) error {
			if !probe && len(args) != 1 {
				return fmt.Errorf("apply-c1 needs the shoot folder (only --probe works without one)")
			}
			var script string
			if probe {
				script = c1.Probe(5)
			} else {
				cfg, err := so.base(args[0])
				if err != nil {
					return err
				}
				rep, err := report.Load(cfg.ReportPath)
				if err != nil {
					return fmt.Errorf("no report: %w (run judge first, or pass -o)", err)
				}
				rep.Relocate(cfg.ReportPath, cfg.Dir) // a renamed shoot folder: match images at their new paths
				if o.Labels, err = userLabels(cmd.ErrOrStderr(), cfg.ReportPath, labelsPath, noLabels); err != nil {
					return err
				}
				if dups := labels.Duplicates(rep.Results); len(o.Labels) > 0 && len(dups) > 0 {
					return fmt.Errorf("frames share a file name, so your labels can't tell them apart (rename them, or pass --no-labels): %s", strings.Join(dups, "; "))
				}
				script = c1.Script(rep, o)
			}
			if !runIt {
				fmt.Fprint(cmd.OutOrStdout(), script)
				return nil
			}
			// osascript reports nothing until Capture One has run the whole script, which
			// takes a while on a big catalogue: a spinner and the elapsed time meanwhile.
			ro := so.out.newOutput(cmd, true)
			t := ui.Track(ro.UI, "apply-c1", "running the script in Capture One", "", 0)
			out, err := c1.Run(osascript, script)
			t.Done()
			ro.Close()
			fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&labelsPath, "labels", "", "your labels log (default: cull-labels.jsonl beside the report, when it exists)")
	f.BoolVar(&noLabels, "no-labels", false, "ignore your labels: the model's verdicts, and no ratings")
	f.BoolVar(&o.Rating, "rating", true, "set star ratings from your labels (only frames you rated)")
	f.BoolVar(&o.Label, "label", true, "set color tags (keep green, review yellow, cull red)")
	f.BoolVar(&o.Keyword, "keyword", true, "apply cull:<verdict> keywords (+ cull:labeled for your verdicts)")
	f.BoolVar(&o.Exposure, "exposure", false, "set the suggested exposure on frames marked fixable")
	f.BoolVar(&o.Crop, "crop", false, "set the suggested crop on frames marked croppable")
	f.BoolVar(&probe, "probe", false, "print a read-only script that lists a few images, to check names and numbering first")
	f.BoolVar(&runIt, "run", false, "pipe the script to osascript instead of printing it")
	f.StringVar(&osascript, "osascript", "osascript", "osascript executable")
	return cmd
}
