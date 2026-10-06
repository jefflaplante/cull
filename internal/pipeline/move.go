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
	"github.com/jefflaplante/cull/internal/ui"
	"github.com/jefflaplante/cull/internal/xmp"
)

// The folders, beside a frame's home, that frames are moved into: the sort folders
// (--sort=culls uses only CullDir). CulledDir is where v0.1 --move-culled put culls;
// it is only read now: reconcileMove and Restore find frames there, and the next sort
// moves them out. Discover skips all of them, so resumed and recursive runs never
// re-process moved frames.
const (
	CulledDir = "culled"
	KeepDir   = "keep"
	ReviewDir = "review"
	CullDir   = "cull"
)

var placeDirs = []string{CulledDir, KeepDir, ReviewDir, CullDir}

// placement is how frames are laid out: culls aside, or every verdict in its folder.
type placement int

const (
	placeNone   placement = iota
	placeCulls            // --sort=culls: culls into cull/, everything else home
	placeSorted           // --sort: keep/, review/, cull/ by verdict
)

// want is the folder beside home where r belongs ("" = home), from its effective
// verdict: the user's label when there is one, else the model's decision. Frames
// that failed stay home.
func want(r report.Result, lab labels.Entry, mode placement) string {
	if r.Error != "" {
		return ""
	}
	d, _ := labels.Effective(r, lab)
	switch mode {
	case placeCulls:
		if d == eval.Cull {
			return CullDir
		}
	case placeSorted:
		switch d {
		case eval.Keep:
			return KeepDir
		case eval.Review:
			return ReviewDir
		case eval.Cull:
			return CullDir
		}
	}
	return ""
}

// place moves frames, with their .xmp sidecars, to where mode wants them: with
// toHome only the ones going home, otherwise only the ones going into (or between)
// folders, so callers can write sidecars in between. Moves are recorded in the
// result; a frame that can't move stays put with the reason in Fixups and a line on
// log. Nothing is deleted or overwritten; `cull restore` reverses it. Returns how
// many moved.
func place(rep *report.Report, lab map[string]labels.Entry, mode placement, log io.Writer, toHome bool, t *ui.Tracker) int {
	n := 0
	for i := range rep.Results {
		r := &rep.Results[i]
		w, cur, src, ok := placeTarget(r, lab, mode, toHome)
		if !ok {
			continue
		}
		t.Add(1)
		dst := r.File
		if w != "" {
			dst = filepath.Join(filepath.Dir(r.File), w, filepath.Base(r.File))
		}
		sidecar, err := relocate(src, dst)
		if err != nil {
			addFixup(r, "move: "+err.Error())
			fmt.Fprintf(log, "not moved %s: %v\n", filepath.Base(r.File), err)
			continue
		}
		followSidecar(r, src, sidecar)
		if cur != "" {
			os.Remove(filepath.Dir(src)) // only succeeds when empty
		}
		r.MovedTo = ""
		if w != "" {
			r.MovedTo = dst
		}
		n++
	}
	return n
}

// placeTarget is where mode wants r (w: "" = the shoot folder, else a folder name),
// which folder it is in now (cur) and its path (src); ok is false when this pass
// leaves it where it is: an error, already in place, or a move for the other pass.
func placeTarget(r *report.Result, lab map[string]labels.Entry, mode placement, toHome bool) (w, cur, src string, ok bool) {
	reconcileMove(r)
	if r.Error != "" {
		return "", "", "", false
	}
	w = want(*r, lab[filepath.Base(r.File)], mode)
	cur, src = "", r.File
	if r.MovedTo != "" {
		cur, src = filepath.Base(filepath.Dir(r.MovedTo)), r.MovedTo
	}
	return w, cur, src, w != cur && toHome == (w == "")
}

// placeShown is place with progress on s: the frames this pass will move are counted
// first, so the stage has a total (none to move: no stage).
func placeShown(rep *report.Report, lab map[string]labels.Entry, mode placement, log io.Writer, toHome bool, s ui.Sink) int {
	total := 0
	for i := range rep.Results {
		if _, _, _, ok := placeTarget(&rep.Results[i], lab, mode, toHome); ok {
			total++
		}
	}
	if total == 0 {
		return 0
	}
	name, text := "sort", "sorting frames into keep/, review/ and cull/"
	switch {
	case toHome:
		name, text = "home", "moving frames back into the shoot folder"
	case mode == placeCulls:
		name, text = "move", "moving culls into cull/"
	}
	t := ui.Track(s, name, text, "frames", total)
	defer t.Done()
	return place(rep, lab, mode, log, toHome, t)
}

// moveCulled moves every frame whose effective verdict is cull, and that isn't
// moved yet, into CullDir beside it (see place; --sort=culls).
func moveCulled(rep *report.Report, lab map[string]labels.Entry, log io.Writer, s ui.Sink) int {
	return placeShown(rep, lab, placeCulls, log, false, s)
}

// Restore moves every frame recorded as moved back to its original path (under
// dir, when the report was written for a folder since renamed), with
// its sidecar, clears the record, saves the report, and removes emptied sort folders
// left empty. It never overwrites: a frame whose original path is taken again
// stays in its sort folder and is reported.
func Restore(reportPath, dir string, log io.Writer, s ui.Sink) (int, error) {
	rep, err := report.Load(reportPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("no report at %s (use -o if it was written elsewhere)", reportPath)
		}
		return 0, err
	}
	rep.Relocate(reportPath, dir) // a renamed shoot folder: its culled/ moved with it
	n := 0
	dirs := map[string]bool{}
	moved := 0
	for _, r := range rep.Results {
		if r.MovedTo != "" {
			moved++
		}
	}
	var t *ui.Tracker
	if moved > 0 {
		t = ui.Track(s, "restore", "moving frames back to where they were", "frames", moved)
		defer t.Done()
	}
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.MovedTo != "" {
			dirs[filepath.Dir(r.MovedTo)] = true
			t.Add(1)
		}
		// Moved or restored by a run whose report was never saved, or moved by hand
		// between folders: restore from wherever it is now.
		reconcileMove(r)
		if r.MovedTo == "" {
			continue
		}
		dirs[filepath.Dir(r.MovedTo)] = true
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
		if rerr := os.Remove(src); rerr != nil {
			os.Remove(dst) // the name made a moment ago: leave nothing a later move would trip on
			return rerr
		}
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	if _, serr := os.Lstat(dst); serr == nil {
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	return os.Rename(src, dst)
}

// reconcileMove repairs a result whose file moved without the report being told: a
// crash or failed save between the renames and the save, or a move by hand between
// the folders. A frame can only be at home or in one of placeDirs beside it, so when
// it isn't where the report says and exactly one of those places holds it (same size
// and mtime: never a stranger), that is where it is. Returns whether r changed.
func reconcileMove(r *report.Result) bool {
	if r.Error != "" {
		return false
	}
	at := r.File
	if r.MovedTo != "" {
		at = r.MovedTo
	}
	if exists(at) {
		return false
	}
	home, base := filepath.Dir(r.File), filepath.Base(r.File)
	var found []string
	for _, c := range append([]string{r.File}, placePaths(home, base)...) {
		if c != at && sameVersion(c, r) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return false
	}
	to := found[0]
	if r.XMP == xmp.Path(at) && !exists(xmp.Path(at)) && exists(xmp.Path(to)) {
		r.XMP = xmp.Path(to)
	}
	r.MovedTo = ""
	if to != r.File {
		r.MovedTo = to
	}
	return true
}

func placePaths(home, base string) []string {
	out := make([]string, len(placeDirs))
	for i, d := range placeDirs {
		out[i] = filepath.Join(home, d, base)
	}
	return out
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// sameVersion reports whether p is the file r describes: a same-disk rename (or
// link) keeps size and mtime, so anything else at p is a stranger, never adopted.
func sameVersion(p string, r *report.Result) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Size() == r.Size && st.ModTime().Equal(r.ModTime)
}

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
