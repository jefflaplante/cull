package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
)

func newRedateCmd(so *sharedOpts) *cobra.Command {
	var day, clock string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "redate <folder>",
		Short: "Fix the capture dates of frames already offloaded (a camera whose clock stopped)",
		Long: `redate sets the capture date of every DNG in a shoot folder, including the frames
sorted into keep/, review/ and cull/ (-r: subfolders too), to --date at --time (local
time). offload --set-date does the same while copying a card.

Each frame's EXIF and embedded XMP dates are rewritten at the same length, digit for
digit, into a hidden copy beside it. That copy is read back from the disk and must
hash to the original's bytes with exactly those fields changed, and it gets the new
file times; only then does it replace the original (an atomic rename). A frame that no
longer matches the checksum offload recorded is refused and left as it is: a changed
or damaged file is never "fixed". Frames with Content Credentials keep their signed
dates; only their file times change.

The offload manifest, the report and cull's sidecars follow, so judge continues the
report without calling the model again. An interrupted run is finished by running the
same command again; until then judge, decide and review refuse. Capture One and
Lightroom may lose track of frames already imported.`,
		Example: `  cull redate ~/Pictures/"2026-10-04 Smith wedding" --date 2026-10-04 --dry-run
  cull redate ~/Pictures/"2026-10-04 Smith wedding" --date 2026-10-04 --time 15:00:00`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if day == "" {
				return errors.New("--date is required: the capture date to set, YYYY-MM-DD")
			}
			target, err := parseLocalTime("--date", day, clock)
			if err != nil {
				return err
			}
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			out := so.out.newOutput(cmd, true)
			defer out.Close()
			res, err := redate.Run(cmd.Context(), redate.Options{Dir: cfg.Dir, ReportPath: cfg.ReportPath, Recursive: so.recursive,
				Target: target, DryRun: dryRun, UI: out.UI})
			out.Close()
			w := cmd.ErrOrStderr()
			if dryRun {
				fmt.Fprintf(w, "dry run, nothing written: would patch %d, file times only %d, already set %d, would refuse %d, values left alone %d\n",
					res.Patched, res.TimesOnly, res.AlreadySet, res.Refused, len(res.Skipped))
			} else {
				fmt.Fprintf(w, "patched %d (each proven), file times only %d, already set %d, refused %d, values left alone %d\n",
					res.Patched, res.TimesOnly, res.AlreadySet, res.Refused, len(res.Skipped))
			}
			if res.Interrupted > 0 {
				fmt.Fprintf(w, "%d replacement(s) interrupted: run redate again to restore them\n", res.Interrupted)
			}
			if _, finish, ok := journal.Incomplete(cfg.Dir); ok && !dryRun {
				if len(res.Orphans) > 0 {
					fmt.Fprintf(w, "unfinished: settle the hidden temp file(s) above, then run %s to finish it\n", finish)
				} else {
					fmt.Fprintf(w, "unfinished: run %s to finish it\n", finish)
				}
			}
			if err != nil {
				return err
			}
			if len(res.Orphans) > 0 {
				status := "cull status " + shellQuote(cfg.Dir)
				if so.recursive {
					status = "cull status -r " + shellQuote(cfg.Dir)
				}
				return fmt.Errorf("%d hidden temp file(s) need you (see the warnings above); %s lists them", len(res.Orphans), status)
			}
			if res.Interrupted > 0 {
				return fmt.Errorf("%d replacement(s) interrupted", res.Interrupted)
			}
			if res.Refused > 0 {
				return fmt.Errorf("%d file(s) refused, left as they are", res.Refused)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&day, "date", "", "the capture date to set, YYYY-MM-DD")
	f.StringVar(&clock, "time", "12:00:00", "the time of day set, HH:MM:SS (local time)")
	f.BoolVar(&dryRun, "dry-run", false, "print what would change, per file; write nothing")
	return cmd
}

// holdShoot takes dir's folder lock, shared, for a command that relies on the frames
// keeping their names and bytes (judge, decide, review, restore, scan, tag, rank,
// import-labels) for its whole run, and refuses while a redate or rename of dir is
// unfinished: its keys may not match the files yet. With recursive (judge -r), every
// folder it reads frames from is held, and checked for unfinished journals. lock false
// (--estimate) takes no lock and makes no lock file. release ends the hold; call it
// when the command is done.
func holdShoot(cmd *cobra.Command, dir string, recursive, lock bool) (release func(), err error) {
	var held []func()
	release = func() {
		for _, h := range held {
			h()
		}
	}
	if lock {
		folders := []string{dir}
		if recursive {
			files, err := pipeline.Discover(dir, true)
			if err != nil {
				return func() {}, err
			}
			seen := map[string]bool{dir: true}
			for _, f := range files {
				if d := filepath.Dir(f); !seen[d] {
					seen[d] = true
					folders = append(folders, d)
				}
			}
		}
		for _, d := range folders {
			h, note, err := journal.Lock(d, false, cmd.Name())
			if err != nil {
				release()
				return func() {}, err
			}
			held = append(held, h)
			if note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+note)
			}
		}
	}
	pending := journal.Unfinished(dir)
	if recursive {
		pending = journal.IncompleteBelow(dir)
	}
	if len(pending) > 0 {
		release()
		p := pending[0]
		msg := fmt.Sprintf("an unfinished %s is recorded in %s: finish it first with %s", p.Which, p.Folder, p.Finish)
		for _, q := range pending[1:] {
			msg += fmt.Sprintf("; and an unfinished %s in %s: %s", q.Which, q.Folder, q.Finish)
		}
		return func() {}, errors.New(msg)
	}
	return release, nil
}
