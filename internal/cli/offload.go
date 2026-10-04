package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

func newOffloadCmd(so *sharedOpts) *cobra.Command {
	var o offload.Options
	var dryRun, noScan, verify bool
	cmd := &cobra.Command{
		Use:   "offload <card>... <dest>",
		Short: "Copy DNGs off camera cards into a shoot folder, verified against the card",
		Long: `offload copies every DNG on the cards into one shoot folder per run,
<dest>/<YYYY-MM-DD> <name>/, dated by the earliest capture date (or --date: camera
clocks get set wrong). The cards are only read.

Every copy is written to a hidden temp file, synced, dropped from the page cache and
read back from the disk, and must match the SHA-256 taken while reading the card
before it gets its real name. Nothing existing is ever replaced. With --backup, the
same single read of the card also fills a second folder, verified separately. A
re-run copies only what is missing (cull-offload.jsonl records what was verified).

"safe to format" is printed only when every file is verified on every destination
and each drive's own write cache was flushed. Then the folder is scanned (free);
judge it next.`,
		Example: `  cull offload /Volumes/LEICA\ M ~/Pictures --name "Smith wedding" --backup /Volumes/Backup/Pictures
  cull offload --dry-run /Volumes/LEICA\ M ~/Pictures --name "Smith wedding"
  cull offload --rename "{date}_{name}_{n:4}" /Volumes/CARD2 ~/Pictures --name "Smith wedding" --date 2026-10-02
  cull offload --verify ~/Pictures/"2026-10-02 Smith wedding"`,
		Args: func(cmd *cobra.Command, args []string) error {
			if verify {
				return cobra.ExactArgs(1)(cmd, args)
			}
			if len(args) < 2 {
				return errors.New("offload needs at least one card and a destination (or --verify <shoot folder>)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if verify {
				return runVerify(cmd, so, args[0])
			}
			o.Sources, o.Dest = args[:len(args)-1], args[len(args)-1]
			tags, err := so.tags.tags() // checked before copying, not after
			if err != nil {
				return err
			}
			po := so.out.newOutput(cmd, true) // reading the cards, or --checksum's hashing, takes a while
			o.UI = po.UI
			plans, err := offload.MakePlans(o)
			po.Close()
			o.UI = nil
			w := cmd.ErrOrStderr()
			if err != nil {
				if len(plans) > 0 {
					fmt.Fprintln(w, plansSummary(plans, o))
				}
				return err
			}
			if dryRun {
				// The plan is a dry run's whole output, so -q doesn't hide it.
				fmt.Fprintln(w, plansSummary(plans, o))
				if tags != nil {
					fmt.Fprintln(w, "tags not stored: --dry-run writes nothing")
				}
				return nil
			}
			out := so.out.newOutput(cmd, true)
			defer out.Close()
			fmt.Fprintln(out.Log, plansSummary(plans, o))
			var results []*offload.Result
			var runErr error
			for _, p := range plans {
				res, err := offload.Run(cmd.Context(), p, out.UI)
				results = append(results, res)
				if err != nil {
					runErr = err
					break // Ctrl-C: the events not started stay uncopied
				}
			}
			out.Close()
			if tags != nil {
				for _, p := range plans {
					if err := storeTags(so, p.Dests[0], tags); err != nil {
						fmt.Fprintf(w, "warning: tags not stored in %s (%v); set them with cull tag\n", p.Folder, err)
					}
				}
			}
			safe, files := len(results) == len(plans), 0
			for i, res := range results {
				mbps := 0.0
				if s := res.Elapsed.Seconds(); s > 0 {
					mbps = float64(res.Bytes) / 1e6 / s
				}
				label := ""
				if len(plans) > 1 {
					label = plans[i].Folder + ": "
				}
				fmt.Fprintf(w, "\n%scopied %d, skipped %d (already there), failed %d: %.1f GB in %s (%.0f MB/s, verified)\n",
					label, res.Copied, res.Skipped, len(res.Failed), float64(res.Bytes)/1e9, res.Elapsed.Round(1e9), mbps)
				safe = safe && res.Safe
				files += len(res.Plan.Files)
			}
			if safe {
				if len(plans) == 1 {
					fmt.Fprintf(w, "all %d files verified on %s: safe to format the card\n", files, strings.Join(plans[0].Dests, " and "))
				} else {
					fmt.Fprintf(w, "all %d files verified in %d shoot folders: safe to format the card\n", files, len(plans))
				}
			} else {
				fmt.Fprintln(w, "NOT safe to format the card:")
				for i, res := range results {
					where := ""
					if len(plans) > 1 {
						where = " (" + plans[i].Folder + ")"
					}
					for _, f := range res.Failed {
						fmt.Fprintf(w, "  failed%s: %s\n", where, f)
					}
					for _, s := range res.SyncErrs {
						fmt.Fprintln(w, "  drive cache not flushed:", s)
					}
					if res.Unverified > 0 {
						fmt.Fprintf(w, "  %d file(s)%s were already there but never checked against the card: rerun with --checksum\n", res.Unverified, where)
					}
				}
				if n := len(plans) - len(results); n > 0 {
					fmt.Fprintf(w, "  %d event(s) not started\n", n)
				}
				if runErr != nil {
					fmt.Fprintln(w, "  stopped:", runErr)
				}
				fmt.Fprintln(w, "rerun the same command to copy what's missing")
				if runErr == nil {
					runErr = errors.New("offload incomplete")
				}
				return runErr
			}
			if noScan {
				return nil
			}
			var scanErr error
			for _, p := range plans {
				scanErr = errors.Join(scanErr, scanShoot(cmd, so, p.Dests[0]))
			}
			return scanErr
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Name, "name", "", "shoot name: the folder is \"<date> <name>\"")
	f.StringVar(&o.Date, "date", "", "folder date YYYY-MM-DD (default: the earliest capture date)")
	f.StringVar(&o.Backup, "backup", "", "also copy into this root (same folder layout), verified separately")
	f.StringVar(&o.Rename, "rename", "", "rename files: {date} {name} {orig} {n} {n:W}, e.g. \"{date}_{name}_{n:4}\"; the counter continues across cards")
	f.BoolVar(&o.Checksum, "checksum", false, "decide what's already copied by SHA-256, not size and time")
	f.BoolVar(&dryRun, "dry-run", false, "print the plan and exit; write nothing")
	f.BoolVar(&noScan, "no-scan", false, "stop after copying (don't scan the shoot folder)")
	f.BoolVar(&o.Split, "split", false, "one shoot folder per event, numbered: a capture-time gap over --split-gap, or a new day, starts the next")
	f.DurationVar(&o.SplitGap, "split-gap", offload.DefaultSplitGap, "with --split, the capture-time gap that starts a new event")
	f.StringSliceVar(&o.SplitAt, "split-at", nil, "start a new event at each of these files (camera order), e.g. M1103402,M1103777; for a card whose clock can't be trusted")
	so.tags.register(f)
	f.BoolVar(&verify, "verify", false, "re-check a shoot folder's copies against the checksums recorded when they were made")
	return cmd
}

func runVerify(cmd *cobra.Command, so *sharedOpts, folder string) error {
	out := so.out.newOutput(cmd, true)
	defer out.Close()
	v, err := offload.Verify(cmd.Context(), folder, out.UI)
	out.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%d verified, %d missing or different\n", v.OK, v.Bad)
	if len(v.Unrecorded) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "%d DNG(s) here were never verified by cull (no manifest line): %s\n", len(v.Unrecorded), strings.Join(v.Unrecorded, ", "))
	}
	switch {
	case v.Bad > 0:
		return fmt.Errorf("%d file(s) in %s don't match the card", v.Bad, folder)
	case len(v.Unrecorded) > 0:
		return fmt.Errorf("%d file(s) in %s can't be vouched for", len(v.Unrecorded), folder)
	}
	return nil
}

// storeTags saves offload's tags in the shoot folder's report, creating one that holds
// only them when the folder has none yet. The scan after the copy would store them
// too, but it doesn't run with --no-scan or after an incomplete copy.
func storeTags(so *sharedOpts, folder string, given *report.Tags) error {
	if _, err := os.Stat(folder); err != nil {
		return nil // nothing was copied, so there is no shoot folder to tag
	}
	cfg, err := so.base(folder)
	if err != nil {
		return err
	}
	rep, err := report.Load(cfg.ReportPath)
	if errors.Is(err, fs.ErrNotExist) {
		rep, err = &report.Report{SchemaVersion: report.SchemaVersion, Dir: cfg.Dir}, nil
	}
	if err != nil {
		return err
	}
	rep.Tags = report.MergeTags(rep.Tags, given)
	return rep.Save(cfg.ReportPath)
}

// scanShoot runs the free scan on a freshly offloaded folder, as `cull scan` would.
func scanShoot(cmd *cobra.Command, so *sharedOpts, folder string) error {
	cfg, err := so.base(folder)
	if err != nil {
		return err
	}
	cfg.DryRun = true
	out := so.out.newOutput(cmd, true)
	defer out.Close()
	out.attach(&cfg)
	rep, usage, err := pipeline.Run(cmd.Context(), cfg, nil)
	out.Close()
	printSummary(cmd, cfg.ReportPath, rep, usage, "", false)
	if rep != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), ScanSummary(rep))
	}
	if err == nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "next: cull judge --estimate %q\n", filepath.Clean(folder))
	}
	return err
}

// plansSummary is the plan as printed: one block per event, under a line saying how
// the card was split when it was.
func plansSummary(plans []*offload.Plan, o offload.Options) string {
	if len(plans) == 1 {
		return plans[0].Summary()
	}
	how := "at " + strings.Join(o.SplitAt, ", ")
	if len(o.SplitAt) == 0 {
		gap := o.SplitGap
		if gap <= 0 {
			gap = offload.DefaultSplitGap
		}
		how = "where capture time jumps by more than " + shortDuration(gap) + " or the day changes"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d events, split %s:\n", len(plans), how)
	for _, p := range plans {
		fmt.Fprintf(&b, "\nevent %d: %s\n", p.Event, p.Summary())
	}
	return strings.TrimRight(b.String(), "\n")
}

// shortDuration drops a duration's trailing zero units: 2h, 1h30m, 45m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
