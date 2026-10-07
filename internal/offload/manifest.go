package offload

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ManifestName is the offload record in each shoot folder: one line per file, written
// only after that file verified on every destination.
const ManifestName = "cull-offload.jsonl"

// Entry is one manifest line.
type Entry struct {
	Src     string    `json:"src"`
	Orig    string    `json:"orig"` // source base name
	Name    string    `json:"name"` // name in the shoot folder
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	SHA256  string    `json:"sha256"` // the card's bytes, always; a patched file adds FileSHA256
	At      time.Time `json:"at"`

	// Set when the file's capture dates were patched (offload --set-date, redate). The
	// patched_at tag is omitzero, not omitempty: a zero time.Time is not "empty" to
	// encoding/json, and unpatched lines stay as they were.
	FileSHA256 string    `json:"file_sha256,omitempty"` // the file as it is now, when its dates were patched
	DatesSet   string    `json:"dates_set,omitempty"`   // "2026-10-04T12:00:00" local: the date set (patched, or only its file times)
	PatchedAt  time.Time `json:"patched_at,omitzero"`
}

// CurrentSHA256 is the checksum the file on disk must have now: FileSHA256 for a patched
// file, else the card's.
func (e Entry) CurrentSHA256() string {
	if e.FileSHA256 != "" {
		return e.FileSHA256
	}
	return e.SHA256
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// readManifest reads folder's manifest; a missing one is empty. A torn last line (a
// crash mid-append) is ignored: its file was never recorded as verified.
func readManifest(folder string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(folder, ManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Name != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// appendManifest records e in folder's manifest as one write, synced: a line exists
// only for a file that verified, and survives a crash right after.
func appendManifest(folder string, e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(folder, ManifestName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := plainSync(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// CurrentManifest is folder's manifest as it stands: the current (last) entry per
// file (see current). A folder without a manifest gives none.
func CurrentManifest(folder string) ([]Entry, error) {
	man, err := readManifest(folder)
	if err != nil {
		return nil, err
	}
	return current(man), nil
}

// AppendManifest records e in folder's manifest, synced: redate's superseding lines.
// The manifest is append-only; the last line per (orig, size) is current.
func AppendManifest(folder string, e Entry) error { return appendManifest(folder, e) }

// DatesSet reads folder's manifest and returns, by file name, the capture date set on
// each file whose current entry records one ("2026-10-04T12:00:00", local): offload
// --set-date and redate. A folder without a manifest gives an empty map.
func DatesSet(folder string) (map[string]string, error) {
	man, err := CurrentManifest(folder)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range man {
		if e.DatesSet != "" {
			out[e.Name] = e.DatesSet
		}
	}
	return out, nil
}
