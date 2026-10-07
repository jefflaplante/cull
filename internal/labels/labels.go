// Package labels is the user's judgement of frames: an append-only JSONL log of
// keep/review/cull labels and star ratings, kept apart from the model's report so
// calibration can compare the two.
package labels

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileName is the log's name, in the report's directory.
const FileName = "cull-labels.jsonl"

// Entry is one frame's full state at one time. The last entry per file wins.
type Entry struct {
	File  string    `json:"file"`         // base name
	Label string    `json:"label"`        // keep | review | cull | "" (unlabelled)
	Stars int       `json:"stars"`        // 0-5; 0 = unrated
	EV    *float64  `json:"ev,omitempty"` // your exposure adjustment from review; nil = none
	At    time.Time `json:"at"`
	// From is the name the frame had when cull rename carried this entry to its new
	// name; "" otherwise.
	From string `json:"from,omitempty"`
}

// Validate checks an entry's fields.
func (e Entry) Validate() error {
	if e.File == "" || e.File == "." || e.File == ".." || e.File != filepath.Base(e.File) {
		return fmt.Errorf("file %q: want a file name without a directory", e.File)
	}
	if e.From != "" && (e.From == "." || e.From == ".." || e.From != filepath.Base(e.From)) {
		return fmt.Errorf("from %q: want a file name without a directory", e.From)
	}
	switch e.Label {
	case "", "keep", "review", "cull":
	default:
		return fmt.Errorf("label %q: want keep, review, cull or empty", e.Label)
	}
	if e.Stars < 0 || e.Stars > 5 {
		return fmt.Errorf("stars %d: want 0-5", e.Stars)
	}
	if e.EV != nil && (*e.EV < -5 || *e.EV > 5 || *e.EV != *e.EV) {
		return fmt.Errorf("ev %v: want -5 to +5", *e.EV)
	}
	return nil
}

// Empty reports whether the entry clears the frame.
func (e Entry) Empty() bool { return e.Label == "" && e.Stars == 0 && e.EV == nil }

// DefaultPath is the log beside a report: one log per shoot directory, shared by
// every report there, since labels describe photos, not a model run.
func DefaultPath(reportPath string) string {
	return filepath.Join(filepath.Dir(reportPath), FileName)
}

// Append adds one entry with a single write, so the log is never rewritten and a
// crash can lose at most the line being written.
func Append(path string, e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if err := healTail(f); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// healTail drops an interrupted append (bytes after the last newline) so the next
// entry doesn't glue onto it: Read already treats such a tail as never written.
// Each append is a single write, so a tail without its newline only comes from a
// crash, never from a writer still in progress.
func healTail(f *os.File) error {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	buf := make([]byte, min(st.Size(), 64*1024))
	off := st.Size() - int64(len(buf))
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	if buf[len(buf)-1] == '\n' {
		return nil
	}
	i := bytes.LastIndexByte(buf, '\n') // -1: the whole window is one torn line
	return f.Truncate(off + int64(i+1))
}

// Read folds the log: the last entry per file wins, and cleared frames are absent.
// A final line without its newline is an interrupted append and is skipped; any
// other bad line is an error, because silently dropping labels would skew
// calibration. A missing log is empty.
func Read(path string) (map[string]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines[:len(lines)-1] { // the last piece is "" or a torn append
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if e.Empty() {
			delete(out, e.File)
		} else {
			out[e.File] = e
		}
	}
	return out, nil
}

// Verdicts returns file → label for the entries that have a label.
func Verdicts(m map[string]Entry) map[string]string {
	v := map[string]string{}
	for f, e := range m {
		if e.Label != "" {
			v[f] = e.Label
		}
	}
	return v
}
