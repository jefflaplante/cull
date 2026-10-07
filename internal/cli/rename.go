package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/rename"
)

func newRenameCmd(so *sharedOpts) *cobra.Command {
	var dryRun, undo, reorder bool
	cmd := &cobra.Command{
		Use:   "rename <folder> <pattern> | rename --undo <folder>",
		Short: "Rename a shoot folder's frames by a pattern (the report, labels and manifest follow)",
		Long: `rename names every DNG in a shoot folder, including the frames sorted into keep/,
review/ and cull/ (each stays in its folder), by a pattern, in camera order: by the
camera's file counter (the last 4 digits of M1103127; the card folder's number first
when the counter wraps) of the card name the offload manifest records, else of the
name now. An M11-P's M… and L… frames share one counter, so they interleave as shot.
Other names (edited copies such as M1103127-Edit) follow, by name. The pattern takes
offload --rename's tokens:

  {date}   the frame's capture date, YYYYMMDD (the date redate or offload --set-date
           set, else EXIF; else the folder name's date)
  {name}   the folder name after its date (spaces become _)
  {orig}   the camera's name for the frame, without its extension
  {n}      a counter from 1 in camera order; {n:4} pads it to 4 digits

The extension is kept (upper case). The sidecar of each frame moves with it.

Nothing moves until the whole plan is checked: two frames that would get one name, or
a new name already held by anything else, refuses the rename. Frames first move to
hidden temp names, then to their new names (so names can swap), journalled in
cull-rename.json. The report, your labels log, the offload manifest and the review
sheet's cache follow, so judge continues the report without calling the model again.

Limits, checked before anything moves (each refuses the rename, naming the files):
  - Only the frame and its <stem>.xmp sidecar move. Other files named for the frame
    (a camera JPG, darktable's <name>.xmp such as M1103817.DNG.xmp, and
    Capture One's settings in CaptureOne/Settings*/<name>.cos) would keep the old
    name and lose their frame.
  - Names over 255 bytes, locked frames and folders that can't be written.
  - A rename that changes the order of frames with the same capture time regroups
    the shoot's sets, and judge would rank them again: --reorder goes ahead anyway.
    Frames the manifest records keep their card names' order whatever they're called;
    only frames it doesn't record go by their new names (an unpadded {n} sorts 10
    before 2; 8-character names ending in 4 digits go by those digits).
  - A pending judge or ranking batch (finish or cancel it first), and a folder another
    cull command is using (the folder lock, .cull.lock).

An interrupted rename is finished by running the same command again, or put back with
--undo; until then the commands that read or change the folder's frames refuse it,
and cull status says why. --undo also puts back the last finished rename. Capture One
and Lightroom may lose track of frames already imported.`,
		Example: `  cull rename ~/Pictures/"2026-10-04 Smith wedding" "{date}_{name}_{n:4}" --dry-run
  cull rename ~/Pictures/"2026-10-04 Smith wedding" "{date}_{name}_{n:4}"
  cull rename --undo ~/Pictures/"2026-10-04 Smith wedding"`,
		Args: func(cmd *cobra.Command, args []string) error {
			if undo {
				if len(args) != 1 {
					return errors.New("rename --undo takes the folder only")
				}
				return nil
			}
			if len(args) != 2 {
				return errors.New(`rename takes the folder and a pattern, e.g. "{date}_{name}_{n:4}" (or --undo and the folder)`)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			pattern := ""
			if !undo {
				pattern = args[1]
			}
			out := so.out.newOutput(cmd, true)
			defer out.Close()
			res, err := rename.Run(cmd.Context(), rename.Options{Dir: cfg.Dir, ReportPath: cfg.ReportPath, Pattern: pattern,
				Recursive: so.recursive, DryRun: dryRun, Undo: undo, Reorder: reorder, UI: out.UI})
			out.Close()
			w := cmd.ErrOrStderr()
			switch {
			case dryRun:
				fmt.Fprintf(w, "dry run, nothing written: would rename %d\n", len(res.Moves))
			case undo && res.Renamed > 0:
				fmt.Fprintf(w, "put back %d frame(s) under the names they had before the rename\n", res.Renamed)
			case res.Renamed > 0:
				fmt.Fprintf(w, "renamed %d frame(s); undo: cull rename --undo %s\n", res.Renamed, shellQuote(cfg.Dir))
			}
			if _, finish, ok := journal.Incomplete(cfg.Dir); ok && !dryRun && err != nil {
				fmt.Fprintf(w, "unfinished: run %s\n", finish)
			}
			return err
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "print old → new, per frame; write nothing")
	f.BoolVar(&reorder, "reorder", false, "go ahead though the new names change the frames' order (the shoot's sets regroup)")
	f.BoolVar(&undo, "undo", false, "put back the names the last rename changed (or finish putting them back)")
	return cmd
}
