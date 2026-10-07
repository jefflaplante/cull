package offload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
)

// The patch proof. A file whose capture dates are fixed must be its source byte for
// byte except at exactly the patched ranges, which hold Patch.New. One read of the
// source gives two hashes: orig (the bytes as read, checked against the card's or the
// manifest's) and want (the same bytes with the patches applied in-stream). After the
// patches are written and synced, the file is evicted and re-read from the device, and
// that hash must equal want.

// checkPatches requires ps sorted by offset, non-overlapping, and each the same length
// before and after: the shape dng.PatchDates returns, which the in-stream apply relies on.
func checkPatches(ps []dng.Patch) error {
	var end int64
	for i, p := range ps {
		if len(p.Old) != len(p.New) {
			return fmt.Errorf("patch %s at %d changes length (%d to %d bytes)", p.What, p.Off, len(p.Old), len(p.New))
		}
		if p.Off < 0 || (i > 0 && p.Off < end) {
			return fmt.Errorf("patches unsorted or overlapping at %d (%s)", p.Off, p.What)
		}
		end = p.Off + int64(len(p.New))
	}
	return nil
}

// streamPatched reads r to EOF, returning the SHA-256 of the bytes as read (orig) and
// of the bytes with ps applied in-stream (want). If w is non-nil the patched bytes are
// written to it. It fails if the bytes at a patch's range don't equal Patch.Old (the
// file isn't the one the patches were computed from) or ps is unsorted/overlapping.
func streamPatched(ctx context.Context, r io.Reader, ps []dng.Patch, w io.Writer) (orig, want [32]byte, err error) {
	if err := checkPatches(ps); err != nil {
		return orig, want, err
	}
	h1, h2 := sha256.New(), sha256.New()
	buf := make([]byte, bufSize)
	var off int64 // file offset of buf[0]
	next := 0     // first patch not yet wholly applied
	for {
		if err := ctx.Err(); err != nil {
			return orig, want, err
		}
		n, rerr := io.ReadFull(r, buf)
		eof := rerr == io.EOF || errors.Is(rerr, io.ErrUnexpectedEOF)
		if rerr != nil && !eof {
			return orig, want, rerr
		}
		c := buf[:n]
		h1.Write(c)
		end := off + int64(n)
		for i := next; i < len(ps) && ps[i].Off < end; i++ {
			p := ps[i]
			// The part of p inside this chunk: file [lo, hi), p's bytes [lo-p.Off, hi-p.Off).
			lo, hi := max(p.Off, off), min(p.Off+int64(len(p.New)), end)
			if lo < hi {
				seg := c[lo-off : hi-off]
				if !bytes.Equal(seg, p.Old[lo-p.Off:hi-p.Off]) {
					return orig, want, fmt.Errorf("%s at %d: the file doesn't hold the bytes the patch was computed from", p.What, p.Off)
				}
				copy(seg, p.New[lo-p.Off:hi-p.Off])
			}
			if p.Off+int64(len(p.New)) <= end {
				next = i + 1
			}
		}
		h2.Write(c)
		if w != nil && n > 0 {
			if _, err := w.Write(c); err != nil {
				return orig, want, err
			}
		}
		off = end
		if eof {
			break
		}
	}
	if next < len(ps) {
		p := ps[next]
		return orig, want, fmt.Errorf("%s at %d runs past the end of the file (%d bytes)", p.What, p.Off, off)
	}
	copy(orig[:], h1.Sum(nil))
	copy(want[:], h2.Sum(nil))
	return orig, want, nil
}

// applyPatches pwrites ps into the file at path (opened O_WRONLY, never O_TRUNC) and
// fsyncs it. A patch reaching past the end is refused before anything is written, so
// the file never changes length.
func applyPatches(path string, ps []dng.Patch) error {
	if err := checkPatches(ps); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	for _, p := range ps {
		if p.Off+int64(len(p.New)) > st.Size() {
			f.Close()
			return fmt.Errorf("%s at %d runs past the end of %s (%d bytes)", p.What, p.Off, path, st.Size())
		}
	}
	if err := noCache(f); err != nil {
		f.Close()
		return fmt.Errorf("uncached write %s: %w", path, err)
	}
	for _, p := range ps {
		if _, err := f.WriteAt(p.New, p.Off); err != nil {
			f.Close()
			return fmt.Errorf("patch %s: %w", path, err)
		}
	}
	if err := plainSync(f); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return f.Close()
}

// ErrNotProven is a proof whose re-read hash differs (as opposed to a read error).
var ErrNotProven = errors.New("read back from disk, it isn't the expected bytes (the source with exactly its date patches applied)")

// proveFrom evicts path from the page cache, re-reads it from the device and
// requires its SHA-256 to equal want (the existing dropCache + hashUncached path).
func proveFrom(ctx context.Context, path string, want [32]byte) error {
	got, err := hashFromDisk(ctx, path)
	if err != nil {
		return fmt.Errorf("prove %s: %w", path, err)
	}
	if got != want {
		return fmt.Errorf("prove %s: %w", path, ErrNotProven)
	}
	return nil
}

// setFileTimes sets path's access and modification times to t and, on darwin, its
// creation time (Setattrlist ATTR_CMN_CRTIME) best effort: crtimeErr reports a
// creation-time failure (the caller notes it once); err is a mtime failure.
func setFileTimes(path string, t time.Time) (crtimeErr, err error) {
	if err := os.Chtimes(path, t, t); err != nil {
		return nil, err
	}
	return setCreationTimeFn(path, t), nil
}

// setCreationTimeFn is setFileTimes' creation-time call; tests swap it to act out a
// filesystem that keeps none.
var setCreationTimeFn = setCreationTime

// current returns, for each (orig, size), the last manifest entry: later lines
// supersede earlier ones (redate and rename append superseding lines). Entries keep
// the order in which each file first appeared.
func current(es []Entry) []Entry {
	type key struct {
		orig string
		size int64
	}
	idx := map[key]int{}
	var out []Entry
	for _, e := range es {
		k := key{strings.ToLower(e.Orig), e.Size}
		if i, ok := idx[k]; ok {
			out[i] = e
			continue
		}
		idx[k] = len(out)
		out = append(out, e)
	}
	return out
}
