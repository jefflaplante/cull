package offload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	bufSize = 4 << 20 // card reads in 4 MiB pieces
	bufs    = 4       // in flight between the reader and the writer
)

// hooks are test seams; zero values mean the real thing.
type hooks struct {
	open         func(string) (io.ReadCloser, error)
	afterWrite   func(tmp string)
	beforeVerify func(tmp string)
	write        func(f *os.File, p []byte) (int, error)
	// evictSource drops a card file from the page cache before each retry of it
	// (default evictPasses), returning how many of its pages stayed cached.
	evictSource func(src string) (resident, pages int, err error)
	// serial makes Run copy one file at a time, each finished before the next card
	// read starts: the engine before the pipeline, for tests and the bench to compare.
	serial bool
}

type chunk struct {
	buf []byte
	n   int
	err error // io.EOF at the end
}

// copyFile reads src once and writes it into every folder in dirs as name, returning
// src's SHA-256 (computed during that one read). A copy appears under name only after
// its temp file is fsynced, re-read without the page cache and matched against the
// hash; nothing existing is ever replaced. On any failure every temp file is removed.
// It is the two stages Run overlaps, one after the other: retries use it.
func copyFile(ctx context.Context, src, name string, dirs []string, size int64, mtime time.Time, h hooks) (sum [32]byte, err error) {
	w, err := writeStage(ctx, src, name, dirs, size, mtime, h)
	if err != nil {
		return sum, err
	}
	return w.sum, finishStage(ctx, w, h)
}

// written is a file after stage A: its card read is done and hashed, and its bytes sit
// in hidden temp files, still open, not yet fsynced or verified. Only finishStage or
// discard may follow.
type written struct {
	name  string
	dirs  []string
	temps []*os.File // one per dir, same order
	size  int64
	mtime time.Time
	sum   [32]byte // the card's bytes, hashed during the one read; never recomputed from a copy
}

// discard closes and removes w's temp files: what every failure or abandonment of a
// file ends with. Closing an already closed file only returns an error.
func (w *written) discard() {
	for _, f := range w.temps {
		f.Close()
		os.Remove(f.Name())
	}
}

// writeStage is stage A: it reads src once, hashing it while writing a hidden temp copy
// into every folder in dirs, and checks the size. Everything here talks to the card;
// nothing is synced. On failure its temps are already removed.
func writeStage(ctx context.Context, src, name string, dirs []string, size int64, mtime time.Time, h hooks) (w *written, err error) {
	in, err := openSource(src, h)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	w = &written{name: name, dirs: dirs, size: size, mtime: mtime, temps: make([]*os.File, 0, len(dirs))}
	defer func() {
		if err != nil {
			w.discard()
			w = nil
		}
	}()
	for _, d := range dirs {
		f, err := createTemp(d, name)
		if err != nil {
			return w, err
		}
		w.temps = append(w.temps, f)
	}

	hs := sha256.New()
	n, err := stream(ctx, in, hs, w.temps, h)
	if err != nil {
		return w, err
	}
	if n != size { // a reader that ends early without an error is still a failed read
		return w, fmt.Errorf("short read of %s: %d of %d bytes", name, n, size)
	}
	copy(w.sum[:], hs.Sum(nil))
	return w, nil
}

// finishStage is stage B: for each copy, in this order, fsync, evict from the page
// cache and check nothing stayed, re-read from the device and compare with the hash
// stage A took from the card, and only then link it to its final name (never replacing
// anything). It doesn't touch the card, so Run overlaps it with the next file's stage
// A. On failure every temp is removed.
func finishStage(ctx context.Context, w *written, h hooks) (err error) {
	defer func() {
		if err != nil {
			w.discard()
		}
	}()
	for _, f := range w.temps {
		if err := syncTemp(f); err != nil {
			return fmt.Errorf("sync %s: %w", f.Name(), err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", f.Name(), err)
		}
		if err := os.Chtimes(f.Name(), w.mtime, w.mtime); err != nil {
			return err
		}
		if err := os.Chmod(f.Name(), 0o644); err != nil {
			return err
		}
		if h.afterWrite != nil {
			h.afterWrite(f.Name())
		}
	}
	for _, f := range w.temps {
		if err := dropCache(f.Name()); err != nil {
			return err
		}
		if h.beforeVerify != nil {
			h.beforeVerify(f.Name())
		}
		got, err := hashUncached(ctx, f.Name())
		if err != nil {
			return fmt.Errorf("verify %s: %w", filepath.Dir(f.Name()), err)
		}
		if got != w.sum {
			return fmt.Errorf("verify %s: the copy in %s differs from the card", w.name, filepath.Dir(f.Name()))
		}
	}
	for i, f := range w.temps {
		final := filepath.Join(w.dirs[i], w.name)
		if err := linkNoReplace(f.Name(), final); err != nil {
			return err
		}
		os.Remove(f.Name())
		syncDir(w.dirs[i])
	}
	return nil
}

// syncTemp is stage B's fsync of each temp copy; tests swap it to inject the errors a
// network share reports there (a full share, an I/O error).
var syncTemp = plainSync

func openSource(src string, h hooks) (io.ReadCloser, error) {
	if h.open != nil {
		return h.open(src)
	}
	// Read-only, and without F_NOCACHE (unlike the temps and the verify read), so the
	// kernel reads ahead. Measured 2026-10-06 on the LEICA M card (exFAT): a plain read
	// streams at 262 MB/s, steady; F_NOCACHE turns read-ahead off and gets ~210 (177–254),
	// and still leaves 98% of the card's pages cached, so it didn't spare the cache
	// either. Nothing here relies on the card's pages being uncached: the hash is taken
	// from the bytes read, and every copy is verified from its own device. A retry evicts
	// the card file first (evictPasses), so it reads the card again, not RAM.
	return os.Open(src)
}

// createTemp makes ".<name>.cull-<random>.tmp" in dir, as rsync does: a crash leaves
// a hidden temp file, never a partial file under the real name.
func createTemp(dir, name string) (*os.File, error) {
	var rb [4]byte
	rand.Read(rb[:])
	p := filepath.Join(dir, "."+name+".cull-"+hex.EncodeToString(rb[:])+".tmp")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := noCache(f); err != nil {
		f.Close()
		os.Remove(p)
		return nil, fmt.Errorf("uncached write %s: %w", p, err)
	}
	return f, nil
}

// stream copies in to every out while hashing, with the reader one goroutine ahead so
// the card keeps streaming while the destinations are written.
func stream(ctx context.Context, in io.Reader, hs hash.Hash, outs []*os.File, h hooks) (total int64, err error) {
	free := make(chan []byte, bufs)
	for i := 0; i < bufs; i++ {
		free <- make([]byte, bufSize)
	}
	chunks := make(chan chunk, bufs)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			n, err := io.ReadFull(in, buf)
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			hs.Write(buf[:n])
			select {
			case chunks <- chunk{buf, n, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	write := h.write
	if write == nil {
		write = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	}
	for c := range chunks {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		for _, f := range outs {
			if _, err := write(f, c.buf[:c.n]); err != nil {
				return total, fmt.Errorf("write %s: %w", f.Name(), err)
			}
		}
		total += int64(c.n)
		free <- c.buf
		switch {
		case c.err == io.EOF:
			return total, nil
		case c.err != nil:
			return total, fmt.Errorf("read card: %w", c.err)
		}
	}
	return total, ctx.Err()
}

// dropCache evicts p's pages from the page cache and checks none remain, so the verify
// read has to come from the device. F_NOCACHE on the write is not enough on its own:
// under load, half of each 4 MiB write stayed cached in some test runs. If pages
// can't be dropped, the copy can't be verified from disk, and it fails rather than
// be checked against RAM.
func dropCache(p string) error {
	r, n, err := evictPasses(p)
	if err != nil {
		return err
	}
	if r != 0 {
		return fmt.Errorf("%d of %d pages of %s stay in the page cache: can't verify the copy from disk", r, n, p)
	}
	return nil
}

// evictPasses evicts p's pages from the page cache and counts what stayed (mincore), in
// up to 5 passes until none did, and returns the last count. It only reads p: the
// file is opened read-only and mapped PROT_READ (mapFile), so it's safe on the card too.
func evictPasses(p string) (resident, pages int, err error) {
	for try := 0; ; try++ {
		if err := evict(p); err != nil {
			return 0, 0, fmt.Errorf("drop %s from the page cache: %w", p, err)
		}
		r, n, err := residentPages(p)
		if err != nil {
			return 0, 0, fmt.Errorf("check %s in the page cache: %w", p, err)
		}
		if r == 0 || try == 4 {
			return r, n, nil
		}
		time.Sleep(time.Duration(try+1) * 20 * time.Millisecond)
	}
}

// beforeDiskRead is a test hook: called after p was dropped from the page cache, just
// before hashFromDisk reads it.
var beforeDiskRead func(p string)

// hashFromDisk drops p from the page cache, checks it's gone, and hashes it from the
// device: how --checksum and --verify read copies.
func hashFromDisk(ctx context.Context, p string) ([32]byte, error) {
	if err := dropCache(p); err != nil {
		return [32]byte{}, err
	}
	if beforeDiskRead != nil {
		beforeDiskRead(p)
	}
	return hashUncached(ctx, p)
}

// hashUncached hashes p, reading it from the device rather than the page cache.
func hashUncached(ctx context.Context, p string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	if err := noCache(f); err != nil {
		return sum, err
	}
	hs := sha256.New()
	buf := make([]byte, bufSize)
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		n, err := f.Read(buf)
		hs.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, err
		}
	}
	copy(sum[:], hs.Sum(nil))
	return sum, nil
}

// linkNoReplace gives tmp the name final, failing if final exists: link(2) refuses an
// existing name, where rename(2) would replace it. Filesystems without hard links
// (exFAT backup drives) fall back to check-then-rename.
func linkNoReplace(tmp, final string) error {
	err := os.Link(tmp, final)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists, not replaced: %w", final, fs.ErrExist)
	}
	if _, serr := os.Lstat(final); serr == nil {
		return fmt.Errorf("%s already exists, not replaced: %w", final, fs.ErrExist)
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return nil
}

// syncDir makes a new directory entry durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return plainSync(d)
}
