package offload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

// ReplacePatched rewrites path with exactly the patches ps (dng.PatchDates' output)
// and its times set to t, replacing the original only once the new bytes are proven:
//
//  1. The original is read once into a hidden temp beside it (".<name>.cull-<rand>.tmp"),
//     the patches applied in-stream, hashing both the bytes as read (orig) and as
//     written (want). If expect (hex) is set and orig differs, ErrChanged.
//  2. The temp gets path's permissions and the times t, and is flushed to the media
//     (F_FULLFSYNC; fsync on a network share).
//  3. It is dropped from the page cache, re-read from the device, and must hash to want.
//  4. beforeSwap (if set) runs: redate journals the swap there. An error stops here.
//  5. The temp is renamed over path (rename(2), same folder: either file, never a torn
//     one), and the folder is flushed to the media.
//
// On any failure before the rename, the temp is removed and path is untouched. A file
// with other hard links, or changed (size, mtime) while it was read, is refused.
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
	tmp, err := createTemp(dir, name)
	if err != nil {
		return r, err
	}
	renamed := false
	defer func() {
		tmp.Close() // closing twice only returns an error
		if !renamed {
			os.Remove(tmp.Name())
		}
	}()
	r.Orig, r.Want, err = streamPatched(ctx, in, ps, tmp)
	if err != nil {
		return r, fmt.Errorf("read %s: %w", path, err)
	}
	if expect != "" && hexOf(r.Orig[:]) != expect {
		return r, ErrChanged
	}
	if tst, err := tmp.Stat(); err != nil || tst.Size() != st.Size() {
		return r, fmt.Errorf("the copy of %s isn't the same size (%v)", path, err)
	}
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
	if beforeSwap != nil {
		if err := beforeSwap(r.Orig, r.Want); err != nil {
			return r, err
		}
	}
	// Narrow the window: the original must still be the file just read.
	if now, err := os.Lstat(path); err != nil || now.Size() != st.Size() || !now.ModTime().Equal(st.ModTime()) || !os.SameFile(now, st) {
		return r, fmt.Errorf("%s changed while it was being fixed; not replaced", path)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return r, err
	}
	renamed = true
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return r, fmt.Errorf("%s replaced, but its folder can't be flushed: %w", path, err)
	}
	defer d.Close()
	if _, err := durable(d); err != nil {
		return r, fmt.Errorf("%s replaced, but its folder can't be flushed: %w", path, err)
	}
	return r, nil
}

// SetFileTimes sets path's modification (and access) time to t and, best effort, its
// creation time: crtimeErr reports a creation-time failure, err a mtime failure.
func SetFileTimes(path string, t time.Time) (crtimeErr, err error) { return setFileTimesFn(path, t) }

// ProveFrom drops path from the page cache, re-reads it from the device, and requires
// its SHA-256 to equal want.
func ProveFrom(ctx context.Context, path string, want [32]byte) error {
	return proveFrom(ctx, path, want)
}

// RemoveStaleTemps removes the hidden temps (".<name>.cull-<rand>.tmp") a crash left in
// dir: never a file's only copy, since a temp takes its name only once proven.
func RemoveStaleTemps(dir string) {
	stale, _ := filepath.Glob(filepath.Join(dir, ".*.cull-*.tmp"))
	for _, s := range stale {
		os.Remove(s)
	}
}
