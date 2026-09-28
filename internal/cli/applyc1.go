package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/c1"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func newApplyC1Cmd(so *sharedOpts) *cobra.Command {
	var (
		o         c1.Options
		probe     bool
		runIt     bool
		osascript string
	)
	cmd := &cobra.Command{
		Use:   "apply-c1 <dir>",
		Short: "Generate (and optionally run) AppleScript that applies the report in Capture One",
		Long: `apply-c1 writes an AppleScript for the open Capture One document: rating, color
tag and a gophotocull:<decision> keyword per frame, and with --exposure / --crop the
suggested exposure and crop. It prints the script by default (a dry run); --run
pipes it to osascript.

Run --probe first: a read-only script that lists a few images with their names,
color tags, dimensions and crops, to confirm how Capture One names and numbers
things before anything is written.`,
		Example: `  gophotocull apply-c1 --probe ~/Pictures/2026-09-26 | osascript -
  gophotocull apply-c1 ~/Pictures/2026-09-26 > apply.applescript   # review it
  gophotocull apply-c1 --run ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
					return fmt.Errorf("no report: %w (run cull first, or pass -o)", err)
				}
				script = c1.Script(rep, o)
			}
			if !runIt {
				fmt.Fprint(cmd.OutOrStdout(), script)
				return nil
			}
			out, err := c1.Run(osascript, script)
			fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.Rating, "rating", true, "set star ratings (keep 3, review 2, cull 1)")
	f.BoolVar(&o.Label, "label", true, "set color tags (review yellow, cull red)")
	f.BoolVar(&o.Keyword, "keyword", true, "apply gophotocull:<decision> keywords")
	f.BoolVar(&o.Exposure, "exposure", false, "set the suggested exposure on frames marked fixable")
	f.BoolVar(&o.Crop, "crop", false, "set the suggested crop on frames marked croppable")
	f.BoolVar(&probe, "probe", false, "print a read-only script that lists a few images, to check names and numbering first")
	f.BoolVar(&runIt, "run", false, "pipe the script to osascript instead of printing it")
	f.StringVar(&osascript, "osascript", "osascript", "osascript executable")
	return cmd
}
