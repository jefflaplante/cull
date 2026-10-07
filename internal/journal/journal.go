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
)

const (
	RedateName = "cull-redate.json" // in the shoot folder while a redate runs
	RenameName = "cull-rename.json" // the last rename, kept for --undo (Task 6)
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

// rename is the part of the rename journal (Task 6) Incomplete reads.
type rename struct {
	Pattern  string `json:"pattern"`
	Complete bool   `json:"complete"`
}

// Incomplete reports an unfinished redate or rename journal in dir: which ("redate"
// or "rename") and the command that finishes it. A journal that can't be read counts
// as unfinished: refusing is the safe side.
func Incomplete(dir string) (which, finish string, ok bool) {
	if j, err := LoadRedate(dir); err != nil {
		return "redate", fmt.Sprintf("cull redate %s --date <the date of the unfinished run> (%s can't be read: %v)", ShellQuote(dir), RedateName, err), true
	} else if j != nil && !j.Complete {
		return "redate", j.Finish(dir), true
	}
	b, err := os.ReadFile(filepath.Join(dir, RenameName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false
	}
	var r rename
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	switch {
	case err != nil:
		return "rename", fmt.Sprintf("cull rename --undo %s (%s can't be read: %v)", ShellQuote(dir), RenameName, err), true
	case !r.Complete:
		return "rename", fmt.Sprintf("cull rename %s %s (or cull rename --undo %s)", ShellQuote(dir), ShellQuote(r.Pattern), ShellQuote(dir)), true
	}
	return "", "", false
}

// IncompleteBelow is Incomplete for dir and every folder below it (hidden ones
// skipped): the first unfinished journal found, and the folder it is in.
func IncompleteBelow(dir string) (folder, which, finish string, ok bool) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if which, finish, ok = Incomplete(p); ok {
			folder = p
			return filepath.SkipAll
		}
		return nil
	})
	return folder, which, finish, ok
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
