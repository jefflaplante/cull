package pipeline

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

// CulledDir is the folder, next to the frames, that --move-culled moves culls
// into. Discover skips it, so resumed and recursive runs never re-process them.
const CulledDir = "culled"

// moveCulled moves every frame whose effective verdict is cull (the user's label
// when lab has one, else the model's decision) and that isn't moved yet into
// CulledDir beside it,
// with its .xmp sidecar, and records the move in the result. Frames that can't
// be moved stay where they are with the reason in Fixups. Nothing is deleted
// and nothing is overwritten; `cull restore` reverses it.
func moveCulled(rep *report.Report, lab map[string]labels.Entry, log io.Writer) int {
	n := 0
	for i := range rep.Results {
		r := &rep.Results[i]
		reconcileMove(r)
		if d, _ := labels.Effective(*r, lab[filepath.Base(r.File)]); d != eval.Cull || r.Error != "" || r.MovedTo != "" {
			continue
		}
		dst := filepath.Join(filepath.Dir(r.File), CulledDir, filepath.Base(r.File))
		sidecar, err := relocate(r.File, dst)
		if err != nil {
			addFixup(r, "move: "+err.Error())
			fmt.Fprintf(log, "not moved %s: %v\n", filepath.Base(r.File), err)
			continue
		}
		followSidecar(r, r.File, sidecar)
		r.MovedTo = dst
		n++
	}
	return n
}

// Restore moves every frame recorded as moved back to its original path, with
// its sidecar, clears the record, saves the report, and removes culled folders
// left empty. It never overwrites: a frame whose original path is taken again
// stays in the culled folder and is reported.
func Restore(reportPath string, log io.Writer) (int, error) {
	rep, err := report.Load(reportPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("no report at %s (use -o if it was written elsewhere)", reportPath)
		}
		return 0, err
	}
	n := 0
	dirs := map[string]bool{}
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.MovedTo != "" {
			dirs[filepath.Dir(r.MovedTo)] = true
		}
		if wasMoved := r.MovedTo != ""; reconcileMove(r) {
			if wasMoved { // restored before, but that report was never saved
				continue
			}
			dirs[filepath.Dir(r.MovedTo)] = true // moved before, never recorded: restore it now
		}
		if r.MovedTo == "" {
			continue
		}
		sidecar, err := relocate(r.MovedTo, r.File)
		if err != nil {
			fmt.Fprintf(log, "not restored %s: %v\n", filepath.Base(r.File), err)
			continue
		}
		followSidecar(r, r.MovedTo, sidecar)
		r.MovedTo = ""
		n++
	}
	for d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
	return n, rep.Save(reportPath)
}

// renameNoReplace moves src to dst and fails with fs.ErrExist if dst exists,
// atomically where the filesystem has hard links: link(2) refuses an existing
// name, while rename(2) silently replaces one. Filesystems without hard links
// (exFAT cards) fall back to check-then-rename. Across disks both fail: this
// never copies.
func renameNoReplace(src, dst string) error {
	err := os.Link(src, dst)
	switch {
	case err == nil:
		return os.Remove(src)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	if _, serr := os.Lstat(dst); serr == nil {
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	return os.Rename(src, dst)
}

// reconcileMove repairs a result whose files moved without the report being
// saved (a crash or a failed save between the renames and the save). A frame's
// place in culled/ is fixed (CulledDir beside it), so where exactly one of the
// two paths exists, that is where it is. Returns whether r changed.
func reconcileMove(r *report.Result) bool {
	if r.Error != "" {
		return false
	}
	from, to := r.File, filepath.Join(filepath.Dir(r.File), CulledDir, filepath.Base(r.File)) // unrecorded move
	if r.MovedTo != "" {
		from, to = r.MovedTo, r.File // unrecorded restore
	}
	if exists(from) || !exists(to) {
		return false
	}
	if r.XMP == xmp.Path(from) && !exists(xmp.Path(from)) && exists(xmp.Path(to)) {
		r.XMP = xmp.Path(to)
	}
	if r.MovedTo == "" {
		r.MovedTo = to
	} else {
		r.MovedTo = ""
	}
	return true
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// relocate renames src to dst and, if src has an .xmp sidecar, that too. Both
// destinations are checked first so the pair moves together or not at all, and
// a failed sidecar rename puts the frame back. Rename only: a move across disks
// fails rather than copying and deleting. Returns the sidecar's new path.
func relocate(src, dst string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	srcXMP, dstXMP := xmp.Path(src), xmp.Path(dst)
	_, err := os.Stat(srcXMP)
	hasXMP := err == nil
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%s already exists, not moved", dst)
	}
	if hasXMP {
		if _, err := os.Stat(dstXMP); err == nil {
			return "", fmt.Errorf("%s already exists, not moved", dstXMP)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := renameNoReplace(src, dst); err != nil {
		return "", err
	}
	if !hasXMP {
		return "", nil
	}
	if err := renameNoReplace(srcXMP, dstXMP); err != nil {
		if rerr := os.Rename(dst, src); rerr != nil {
			return "", fmt.Errorf("sidecar: %v; and could not move the frame back: %v", err, rerr)
		}
		return "", fmt.Errorf("sidecar: %w", err)
	}
	return dstXMP, nil
}

// followSidecar updates the report's claim on a sidecar that moved with its
// frame from `from`. Only a sidecar that was ours stays ours: a foreign one
// travels with its frame but never becomes something decide may overwrite.
func followSidecar(r *report.Result, from, newSidecar string) {
	if newSidecar == "" {
		return
	}
	if r.XMP == xmp.Path(from) {
		r.XMP = newSidecar
	} else {
		r.XMP = ""
	}
}
