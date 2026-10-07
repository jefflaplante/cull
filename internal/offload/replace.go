package offload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
)

// What cull redate needs from the copy engine, so a shoot folder's files are fixed by
// the same code that proves offload's copies: the patch stream, the uncached proof,
// the file times, the hidden temps and the drive flush.

// ErrChanged is ReplacePatched's refusal of a file whose bytes no longer hash to the
// checksum its manifest records: a file changed or damaged since, never "fixed".
var ErrChanged = errors.New("no longer matches its offload checksum: not fixing a changed or damaged file")

// Replaced is what ReplacePatched did.
type Replaced struct {
	Orig, Want [32]byte // the file as read, and with the patches applied (the file now)
	CrtimeErr  error    // setting the creation time failed (best effort); the mtime is set
	XattrErrs  []error  // extended attributes that couldn't be copied (best effort)
	// Swapped: the proven temp was renamed over the original. False with an error means
	// the original is untouched.
	Swapped bool
	// TempKept: the swap failed after the original's name was gone (a rename-over that
	// deletes first), so the proven temp was kept: it may be the only copy.
	TempKept bool
	Temp     string // the kept temp's path, with TempKept
	Proven   bool   // the temp re-read from the disk as want
}

// RedateTempPrefix starts ReplacePatched's temps: ".cull-redate-<8 hex>.<name>". After a
// crash a redate temp may be a file's only copy (a rename-over that deletes first,
// interrupted), so only RedateTemps' caller, which can prove it, decides its fate:
//   - offload's sweep (RemoveStaleTemps, ".<name>.cull-<8 hex>.tmp") never takes one;
//   - it never starts "._", so it can't pass for an AppleDouble companion, whatever the
//     frame's name (Pentax "_IGP0001.DNG", Nikon and Sony "_DSC", Canon "_MG_"), which
//     recovery would skip and dot_clean or find -name '._*' -delete would remove.
const RedateTempPrefix = ".cull-redate-"

var redateTempRE = regexp.MustCompile(`^` + regexp.QuoteMeta(RedateTempPrefix) + `[0-9a-f]{8}\.([^.].*)$`) // redate never takes a hidden file

// ReplacePatched rewrites path with exactly the patches ps (dng.PatchDates' output)
// and its times set to t, replacing the original only once the new bytes are proven:
//
//  1. The original is read once into a hidden temp beside it (".cull-redate-<rand>.<name>"),
//     the patches applied in-stream, hashing both the bytes as read (orig) and as
//     written (want). If expect (hex) is set and orig differs, ErrChanged.
//  2. beforeSwap (if set) runs: redate journals the swap there, before the flush below,
//     which carries it to the media too. An error stops here.
//  3. The temp gets path's permissions, extended attributes and the times t, and is
//     flushed to the media (F_FULLFSYNC; fsync on a network share).
//  4. It is dropped from the page cache, re-read from the device, and must hash to want.
//  5. The temp is renamed over path (rename(2), same folder), and the folder is flushed
//     to the media.
//
// On any failure before the rename, the temp is removed and path is untouched, except
// that a temp is never removed once path's name is gone: it may then be the only copy.
// A file with other hard links, or changed (size, mtime) while it was read, is refused.
func ReplacePatched(ctx context.Context, path string, ps []dng.Patch, expect string, t time.Time, beforeSwap func(orig, want [32]byte) error) (r Replaced, err error) {
	in, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return r, err
	}
	if !st.Mode().IsRegular() {
		return r, fmt.Errorf("%s isn't a regular file", path)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && uint64(s.Nlink) > 1 {
		return r, fmt.Errorf("%s has %d hard links: replacing it would leave the others with the old dates", path, s.Nlink)
	}
	dir, name := filepath.Split(path)
	tmp, err := createNamedTemp(dir, func(rnd string) string { return RedateTempPrefix + rnd + "." + name })
	if err != nil {
		return r, err
	}
	defer func() {
		tmp.Close() // closing twice only returns an error
		if r.Swapped {
			return
		}
		if _, err := os.Lstat(path); err == nil {
			os.Remove(tmp.Name())
		} else { // never the only copy
			r.TempKept, r.Temp = true, tmp.Name()
		}
	}()
	r.Orig, r.Want, err = streamPatched(ctx, in, ps, tmp)
	if err != nil {
		return r, fmt.Errorf("read %s: %w", path, err)
	}
	if afterStream != nil {
		afterStream(path)
	}
	if expect != "" && hexOf(r.Orig[:]) != expect {
		return r, ErrChanged
	}
	if tst, err := tmp.Stat(); err != nil || tst.Size() != st.Size() {
		return r, fmt.Errorf("the copy of %s isn't the same size (%v)", path, err)
	}
	if beforeSwap != nil {
		if err := beforeSwap(r.Orig, r.Want); err != nil {
			return r, err
		}
	}
	// Attributes first: once the temp takes a read-only original's mode, some can't be set.
	r.XattrErrs = copyXattrs(in, tmp)
	_ = tmp.Chmod(st.Mode().Perm()) // best effort: exFAT and some shares refuse modes
	// The times go on before the flush, with nothing written after them.
	if r.CrtimeErr, err = setFileTimesFn(tmp.Name(), t); err != nil {
		return r, err
	}
	if _, err := durable(tmp); err != nil {
		return r, fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return r, fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := proveFrom(ctx, tmp.Name(), r.Want); err != nil {
		return r, err
	}
	r.Proven = true
	// Narrow the window: the original must still be the file just read.
	if now, err := os.Lstat(path); err != nil || now.Size() != st.Size() || !now.ModTime().Equal(st.ModTime()) || !os.SameFile(now, st) {
		return r, fmt.Errorf("%s changed while it was being fixed; not replaced", path)
	}
	if err := renameFn(tmp.Name(), path); err != nil {
		return r, err
	}
	r.Swapped = true
	if err := Flush(filepath.Clean(dir)); err != nil {
		return r, fmt.Errorf("%s replaced, but its folder can't be flushed: %w", path, err)
	}
	return r, nil
}

// RedateTemps lists dir's redate temps, each with the path it was to replace. Names
// are matched one by one (not by a glob on the path: a folder may be named
// "trip [day 1]").
func RedateTemps(dir string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if sm := redateTempRE.FindStringSubmatch(e.Name()); sm != nil && e.Type().IsRegular() && !appleDouble(e.Name()) {
			out[filepath.Join(dir, e.Name())] = filepath.Join(dir, sm[1])
		}
	}
	return out
}

// Adopt gives a proven redate temp its file's name, which must be free (never
// replacing anything), and flushes the folder.
func Adopt(tmp, target string) error {
	if err := linkNoReplace(tmp, target); err != nil {
		return err
	}
	os.Remove(tmp) // after a link, the spare name; after the fallback rename, nothing
	return Flush(filepath.Dir(target))
}

// Flush carries path (a file or a folder) to the media: F_FULLFSYNC, or fsync where
// that isn't supported (a network share).
func Flush(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = durable(f)
	return err
}

// SetFileTimes sets path's modification (and access) time to t and, best effort, its
// creation time: crtimeErr reports a creation-time failure, err a mtime failure.
func SetFileTimes(path string, t time.Time) (crtimeErr, err error) { return setFileTimesFn(path, t) }

// HashFromDisk drops path from the page cache, checks it's gone, and hashes it from the
// device.
func HashFromDisk(ctx context.Context, path string) ([32]byte, error) { return hashFromDisk(ctx, path) }

// ProveFrom drops path from the page cache, re-reads it from the device, and requires
// its SHA-256 to equal want.
func ProveFrom(ctx context.Context, path string, want [32]byte) error {
	return proveFrom(ctx, path, want)
}

// RemoveStaleTemps removes the hidden temps (".<name>.cull-<rand>.tmp") an offload crash
// left in dir: never a file's only copy, since a copy takes its name (by link, never
// replacing) only once proven. Redate's and rename's temps (RedateTempPrefix,
// RenameTempPrefix) are never touched here.
func RemoveStaleTemps(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if offloadTempRE.MatchString(n) && !redateTempRE.MatchString(n) && !renameTempRE.MatchString(n) && e.Type().IsRegular() && !appleDouble(n) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// offloadTempRE matches createTemp's names, as the glob ".*.cull-*.tmp" did.
var offloadTempRE = regexp.MustCompile(`^\..*\.cull-.*\.tmp$`)

// renameFn is the swap's rename(2); SetRenameHook replaces it in tests.
var renameFn = os.Rename

// SetRenameHook is a test seam: f replaces the rename(2) that swaps a proven temp in
// (to act out a filesystem whose rename-over fails half way). restore puts it back.
func SetRenameHook(f func(old, new string) error) (restore func()) {
	renameFn = f
	return func() { renameFn = os.Rename }
}

// afterStream is a test seam: called with path once the original has been read into
// the temp (SetAfterStreamHook).
var afterStream func(path string)

// SetAfterStreamHook is a test seam: f runs once the original has been read into the
// temp, before anything is checked or journalled. restore removes it.
func SetAfterStreamHook(f func(path string)) (restore func()) {
	afterStream = f
	return func() { afterStream = nil }
}

// appleDouble reports macOS's "._" companion (its extended attributes, on exFAT/FAT) of
// a hidden temp: "._" + ".<name>…" starts "._.". Only that prefix: an offload temp of a
// frame named "_IGP0001.DNG" is "._IGP0001.DNG.cull-<hex>.tmp", a temp to sweep. (A
// frame named "_.<…>" would have its offload temp left behind: harmless.) The
// filesystem removes a companion with its file.
func appleDouble(name string) bool { return strings.HasPrefix(name, "._.") }
