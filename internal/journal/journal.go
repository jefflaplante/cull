// Package journal holds the records that commands changing files in a shoot folder
// (redate, rename) keep while they run, so an interrupted run can be finished, and so
// commands that rely on the report's keys (judge, decide, review) refuse meanwhile.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/cull/internal/labels"
)

const (
	RedateName = "cull-redate.json" // in the shoot folder while a redate runs
	RenameName = "cull-rename.json" // the last rename, kept for --undo
)

// isoLocal is how a redate target is written: local wall-clock time.
const isoLocal = "2006-01-02T15:04:05"

// Redate is an unfinished redate. It is written before the first file changes and
// again before each file does, and removed when the run completes.
type Redate struct {
	Target    string    `json:"target"` // "2026-10-04T12:00:00", local
	Started   time.Time `json:"started"`
	Recursive bool      `json:"recursive,omitempty"`
	Report    string    `json:"report,omitempty"` // the report it updates, when not the folder's default
	// Files are the files this run changed, or is about to, whose bookkeeping isn't
	// saved yet, by path relative to the folder. A re-run uses them to finish a file
	// whose swap happened just before an interruption.
	Files map[string]FileState `json:"files,omitempty"`
	// Done are the files fully recorded (manifest, report saved, sidecar).
	Done     []string `json:"done"`
	Complete bool     `json:"complete"`
}

// FileState is a file as it was before redate changed it, and what it was to become.
type FileState struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Orig    string    `json:"orig,omitempty"` // SHA-256 of the bytes before (patched files)
	Want    string    `json:"want,omitempty"` // SHA-256 with the date patches; "" = only its times changed
}

// LoadRedate reads dir's redate journal; none gives nil, nil.
func LoadRedate(dir string) (*Redate, error) {
	b, err := os.ReadFile(filepath.Join(dir, RedateName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Redate
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, RedateName), err)
	}
	return &j, nil
}

// Save writes the journal atomically: a temp file, fsynced, renamed into place, the
// folder fsynced. On macOS fsync stops at the drive's cache. Redate relies on what it
// does next: a file's record is saved before that file's temp gets F_FULLFSYNC
// (offload.ReplacePatched calls back before its flush), and F_FULLFSYNC empties the
// whole drive cache, this record included, before the swap; a times-only change isn't
// destructive. (On a network share both fall back to fsync, the most a share offers.)
func (j *Redate) Save(dir string) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, RedateName, b)
}

// RemoveRedate deletes dir's redate journal: the run is complete. None is fine.
func RemoveRedate(dir string) error {
	err := os.Remove(filepath.Join(dir, RedateName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

// Finish is the command that finishes this redate of dir.
func (j *Redate) Finish(dir string) string {
	cmd := "cull redate"
	if j.Recursive {
		cmd += " -r"
	}
	if j.Report != "" {
		cmd += " -o " + ShellQuote(j.Report)
	}
	cmd += " " + ShellQuote(dir)
	t, err := time.ParseInLocation(isoLocal, j.Target, time.Local)
	if err != nil {
		return cmd + " --date <the date of the unfinished run>"
	}
	cmd += " --date " + t.Format(time.DateOnly)
	if c := t.Format(time.TimeOnly); c != "12:00:00" {
		cmd += " --time " + c
	}
	return cmd
}

// FileRecord is the journal's record of the file at rel (relative to the folder); a nil
// journal has none.
func (j *Redate) FileRecord(rel string) (FileState, bool) {
	if j == nil {
		return FileState{}, false
	}
	rec, ok := j.Files[rel]
	return rec, ok
}

// Rename is a rename's record (cull-rename.json), written and flushed before the first
// file moves. A rename moves every frame (and its sidecar) to a hidden temp beside it
// (phase 1), then every temp to its new name (phase 2), so frames can swap names; then
// the report, labels log, manifest and review cache follow. It is kept once complete,
// so --undo can reverse the last rename; an undo is journalled the same way (Undo),
// and removed when it completes.
type Rename struct {
	Pattern   string       `json:"pattern"`
	Undo      bool         `json:"undo,omitempty"`
	Recursive bool         `json:"recursive,omitempty"`
	Report    string       `json:"report,omitempty"` // the report it updates, when not the folder's default
	Started   time.Time    `json:"started"`
	Moves     []RenameMove `json:"moves"`
	// Phase is where the files stand: 1 moving to their temps, 2 moving from the temps
	// to the new names (every old name is free by then).
	Phase int `json:"phase"`
	// ReportAt is which paths the report holds for the moved frames: "" the old ones,
	// "temp" the temps', "new" the new ones. It takes them in two saves, so names that
	// swap never mix; a re-run tells from the report itself which save a crash beat.
	ReportAt string `json:"report_at,omitempty"`
	Complete bool   `json:"complete"`
}

// Temps are the journal's temp paths (frames' and sidecars'), relative to the folder.
func (j *Rename) Temps() map[string]bool {
	out := map[string]bool{}
	if j == nil {
		return out
	}
	for _, m := range j.Moves {
		out[m.Tmp] = true
		if m.SidecarTmp != "" {
			out[m.SidecarTmp] = true
		}
	}
	return out
}

// RenameMove is one frame's move, by paths relative to the folder, and the frame as it
// was when the journal was written: a re-run finds it at Old, Tmp or New, and checks
// that a file there is this frame (same size and modification time: a rename keeps
// both).
type RenameMove struct {
	Old        string    `json:"old"`
	Tmp        string    `json:"tmp"`
	New        string    `json:"new"`
	SidecarOld string    `json:"sidecar_old,omitempty"` // set when the frame had a sidecar
	SidecarTmp string    `json:"sidecar_tmp,omitempty"`
	SidecarNew string    `json:"sidecar_new,omitempty"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mtime"`
	// Orig is the camera name the offload manifest records for the frame (with Size, its
	// manifest key); "" for a frame no manifest records.
	Orig     string `json:"orig,omitempty"`
	OrigSize int64  `json:"orig_size,omitempty"` // the manifest entry's size: with Orig, its key
	// Label is the frame's entry in the labels log when the journal was written: the
	// new name gets it (nil: none, so a label left under the new name is cleared).
	Label *labels.Entry `json:"label,omitempty"`
}

// LoadRename reads dir's rename journal; none gives nil, nil.
func LoadRename(dir string) (*Rename, error) {
	b, err := os.ReadFile(filepath.Join(dir, RenameName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Rename
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, RenameName), err)
	}
	return &j, nil
}

// Save writes the rename journal atomically and fsyncs it; the caller then flushes it
// to the media (F_FULLFSYNC) before moving anything.
func (j *Rename) Save(dir string) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, RenameName, b)
}

// RemoveRename deletes dir's rename journal (an undo completed). None is fine.
func RemoveRename(dir string) error {
	err := os.Remove(filepath.Join(dir, RenameName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

// PendingBatch refuses (an error saying what to do) while a judge or ranking batch for
// the report at reportPath is pending: its requests name the frames by path, and its
// results land by those paths.
func PendingBatch(reportPath string) error {
	for _, b := range []struct{ suffix, what string }{{".batch.json", "judge"}, {".rank-batch.json", "ranking"}} {
		if _, err := os.Stat(reportPath + b.suffix); err == nil {
			return fmt.Errorf("a %s batch is pending (%s): finish it with cull judge --batch, or cancel it, first; its results are matched to the frames by their paths", b.what, reportPath+b.suffix)
		}
	}
	return nil
}

// Finish is the command that finishes this rename of dir (and, for a forward rename,
// the one that reverses it instead).
func (j *Rename) Finish(dir string) string {
	opts := ""
	if j.Recursive {
		opts += " -r"
	}
	if j.Report != "" {
		opts += " -o " + ShellQuote(j.Report)
	}
	undo := "cull rename --undo" + opts + " " + ShellQuote(dir)
	if j.Undo {
		return undo
	}
	return "cull rename" + opts + " " + ShellQuote(dir) + " " + ShellQuote(j.Pattern) + " (or " + undo + ")"
}

// Incomplete reports an unfinished redate or rename journal in dir: which ("redate"
// or "rename") and the command that finishes it. A journal that can't be read counts
// as unfinished: refusing is the safe side.
func Incomplete(dir string) (which, finish string, ok bool) {
	if ps := Unfinished(dir); len(ps) > 0 {
		return ps[0].Which, ps[0].Finish, true
	}
	return "", "", false
}

// Pending is an unfinished journal: its folder, which ("redate", "rename") and the
// command that finishes it.
type Pending struct{ Folder, Which, Finish string }

// Unfinished lists every unfinished journal in dir: a redate's and a rename's.
func Unfinished(dir string) []Pending {
	var out []Pending
	if j, err := LoadRedate(dir); err != nil {
		out = append(out, Pending{dir, "redate", fmt.Sprintf("cull redate %s --date <the date of the unfinished run> (%s can't be read: %v)", ShellQuote(dir), RedateName, err)})
	} else if j != nil && !j.Complete {
		out = append(out, Pending{dir, "redate", j.Finish(dir)})
	}
	switch r, err := LoadRename(dir); {
	case err != nil:
		out = append(out, Pending{dir, "rename", renameUnreadable(dir, err)})
	case r != nil && !r.Complete:
		out = append(out, Pending{dir, "rename", r.Finish(dir)})
	}
	return out
}

// renameUnreadable is what to do about a rename journal that can't be read: frames may
// be under hidden temps it alone records, so it must be mended, never removed.
func renameUnreadable(dir string, err error) string {
	return fmt.Sprintf("cull status %s, which lists the hidden .cull-rename- temps (%s can't be read: %v; restore it from a backup rather than delete it: frames may be under those temps)", ShellQuote(dir), RenameName, err)
}

// IncompleteBelow lists the unfinished journals of every kind in dir and every folder
// below it (hidden ones skipped).
func IncompleteBelow(dir string) []Pending {
	var out []Pending
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		out = append(out, Unfinished(p)...)
		return nil
	})
	return out
}

// safeShell are the characters a shell argument can hold unquoted.
var safeShell = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// ShellQuote makes s safe to paste into a POSIX shell: single quotes unless it needs
// none.
func ShellQuote(s string) string {
	if safeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeAtomic(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, nothing is left to remove
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := syscall.Fsync(int(f.Fd())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o644) // CreateTemp makes 0600; best effort on shares
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := syscall.Fsync(int(d.Fd())); err != nil && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}
