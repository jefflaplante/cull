package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
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
			p, err := offload.MakePlan(o)
			if err != nil {
				if p != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), p.Summary())
				}
				return err
			}
			out := so.out.newOutput(cmd, !dryRun)
			defer out.Close()
			fmt.Fprintln(out.Log, p.Summary())
			if dryRun {
				return nil
			}
			res, runErr := offload.Run(cmd.Context(), p, out.UI)
			out.Close()
			w := cmd.ErrOrStderr()
			mbps := 0.0
			if s := res.Elapsed.Seconds(); s > 0 {
				mbps = float64(res.Bytes) / 1e6 / s
			}
			fmt.Fprintf(w, "\ncopied %d, skipped %d (already there), failed %d: %.1f GB in %s (%.0f MB/s, verified)\n",
				res.Copied, res.Skipped, len(res.Failed), float64(res.Bytes)/1e9, res.Elapsed.Round(1e9), mbps)
			if res.Safe {
				fmt.Fprintf(w, "all %d files verified on %s: safe to format the card\n", len(p.Files), strings.Join(p.Dests, " and "))
			} else {
				fmt.Fprintln(w, "NOT safe to format the card:")
				for _, f := range res.Failed {
					fmt.Fprintln(w, "  failed:", f)
				}
				for _, s := range res.SyncErrs {
					fmt.Fprintln(w, "  drive cache not flushed:", s)
				}
				if res.Unverified > 0 {
					fmt.Fprintf(w, "  %d file(s) were already there but never checked against the card: rerun with --checksum\n", res.Unverified)
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
			return scanShoot(cmd, so, p.Dests[0])
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
	f.BoolVar(&verify, "verify", false, "re-check a shoot folder's copies against the checksums recorded when they were made")
	return cmd
}

func runVerify(cmd *cobra.Command, so *sharedOpts, folder string) error {
	out := so.out.newOutput(cmd, true)
	defer out.Close()
	ok, bad, err := offload.Verify(cmd.Context(), folder, out.UI)
	out.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%d verified, %d missing or different\n", ok, bad)
	if bad > 0 {
		return fmt.Errorf("%d file(s) in %s don't match the card", bad, folder)
	}
	return nil
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
