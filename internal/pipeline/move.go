package pipeline

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
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
		if d, _ := labels.Effective(*r, lab[filepath.Base(r.File)]); d != eval.Cull || r.Error != "" || r.MovedTo != "" {
			continue
		}
		dst := filepath.Join(filepath.Dir(r.File), CulledDir, filepath.Base(r.File))
		sidecar, err := relocate(r.File, dst)
		if err != nil {
			r.Fixups = append(r.Fixups, "move: "+err.Error())
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
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	if !hasXMP {
		return "", nil
	}
	if err := os.Rename(srcXMP, dstXMP); err != nil {
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
