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
	SHA256  string    `json:"sha256"`
	At      time.Time `json:"at"`
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
