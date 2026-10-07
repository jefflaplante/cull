package offload

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// What cull rename needs from the copy engine: its hidden temp names, and a move that
// never replaces anything.

// RenameTempPrefix starts cull rename's temps: ".cull-rename-<8 hex>.<name>", a frame or
// its sidecar between its old and new names. Like redate's (RedateTempPrefix), the name
// never starts "._", so it can't pass for an AppleDouble companion whatever the frame
// is called, offload's sweep never takes one, and a hidden file is never judged.
const RenameTempPrefix = ".cull-rename-"

var renameTempRE = regexp.MustCompile(`^` + regexp.QuoteMeta(RenameTempPrefix) + `[0-9a-f]{8}\.([^.].*)$`)

// RenameTempName is a new temp name for name (a base name): random, so it is free.
func RenameTempName(name string) string {
	var rb [4]byte
	rand.Read(rb[:])
	return RenameTempPrefix + hex.EncodeToString(rb[:]) + "." + name
}

// RenameTemps lists dir's rename temps, each with the name it was made for. Names are
// matched one by one, never by a glob on the path; AppleDouble companions are skipped.
func RenameTemps(dir string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if sm := renameTempRE.FindStringSubmatch(e.Name()); sm != nil && e.Type().IsRegular() && !appleDouble(e.Name()) {
			out[filepath.Join(dir, e.Name())] = sm[1]
		}
	}
	return out
}

// MoveNoReplace gives src the name dst, which must be free: a hard link (link(2) refuses
// an existing name), then src's name is removed; where there are no hard links (exFAT,
// FAT), a check, then rename(2). It never replaces anything. src's name is removed only
// once dst is the same file. The caller flushes the folder.
func MoveNoReplace(src, dst string) error {
	if err := linkNoReplace(src, dst); err != nil {
		return err
	}
	a, err := os.Lstat(src)
	if err != nil {
		return nil // the fallback renamed it
	}
	b, err := os.Lstat(dst)
	if err != nil || !os.SameFile(a, b) {
		return fmt.Errorf("%s was linked as %s, but they aren't the same file now; both kept", src, dst)
	}
	return os.Remove(src)
}
