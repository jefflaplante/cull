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
func copyFile(ctx context.Context, src, name string, dirs []string, size int64, mtime time.Time, h hooks) (sum [32]byte, err error) {
	in, err := openSource(src, h)
	if err != nil {
		return sum, err
	}
	defer in.Close()

	temps := make([]*os.File, 0, len(dirs))
	defer func() {
		if err != nil {
			for _, f := range temps {
				f.Close()
				os.Remove(f.Name())
			}
		}
	}()
	for _, d := range dirs {
		f, err := createTemp(d, name)
		if err != nil {
			return sum, err
		}
		temps = append(temps, f)
	}

	hs := sha256.New()
	n, err := stream(ctx, in, hs, temps, h)
	if err != nil {
		return sum, err
	}
	if n != size { // a reader that ends early without an error is still a failed read
		return sum, fmt.Errorf("short read of %s: %d of %d bytes", name, n, size)
	}
	copy(sum[:], hs.Sum(nil))

	for _, f := range temps {
		if err := plainSync(f); err != nil {
			return sum, fmt.Errorf("sync %s: %w", f.Name(), err)
		}
		if err := f.Close(); err != nil {
			return sum, fmt.Errorf("close %s: %w", f.Name(), err)
		}
		if err := os.Chtimes(f.Name(), mtime, mtime); err != nil {
			return sum, err
		}
		if err := os.Chmod(f.Name(), 0o644); err != nil {
			return sum, err
		}
		if h.afterWrite != nil {
			h.afterWrite(f.Name())
		}
	}
	for _, f := range temps {
		if err := dropCache(f.Name()); err != nil {
			return sum, err
		}
		if h.beforeVerify != nil {
			h.beforeVerify(f.Name())
		}
		got, err := hashUncached(ctx, f.Name())
		if err != nil {
			return sum, fmt.Errorf("verify %s: %w", filepath.Dir(f.Name()), err)
		}
		if got != sum {
			return sum, fmt.Errorf("verify %s: the copy in %s differs from the card", name, filepath.Dir(f.Name()))
		}
	}
	for i, f := range temps {
		final := filepath.Join(dirs[i], name)
		if err := linkNoReplace(f.Name(), final); err != nil {
			return sum, err
		}
		os.Remove(f.Name())
		syncDir(dirs[i])
	}
	return sum, nil
}

func openSource(src string, h hooks) (io.ReadCloser, error) {
	if h.open != nil {
		return h.open(src)
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	noCache(f) // best effort: the card's pages are read once; keeping them only evicts others
	return f, nil
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
	for try := 0; ; try++ {
		if err := evict(p); err != nil {
			return fmt.Errorf("drop %s from the page cache: %w", p, err)
		}
		r, n, err := residentPages(p)
		if err != nil {
			return fmt.Errorf("check %s in the page cache: %w", p, err)
		}
		if r == 0 {
			return nil
		}
		if try == 4 {
			return fmt.Errorf("%d of %d pages of %s stay in the page cache: can't verify the copy from disk", r, n, p)
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
