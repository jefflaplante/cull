//go:build darwin

package offload

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// When the verify read starts, none of the copy's pages may be in the page cache:
// otherwise the check is served from RAM and says nothing about the disk. F_NOCACHE
// alone isn't enough under load (half of each 4 MiB write stayed cached in some runs),
// hence the explicit eviction this checks.
func TestVerifyStartsWithNothingCached(t *testing.T) {
	src, _ := randomFile(t, 32<<20)
	dst := t.TempDir()
	checked := 0
	h := hooks{
		afterWrite: func(tmp string) { warmCache(t, tmp) }, // the worst case: every page cached
		beforeVerify: func(tmp string) {
			r, n, err := residentPages(tmp)
			if err != nil {
				t.Fatal(err)
			}
			// dropCache saw 0 pages resident; nothing writes the copy after that, so a page
			// back in the cache now was read from the device by someone else: a file
			// scanner (Spotlight, XProtect) reading new files' first and last pages, seen
			// as pages [0 1] and [0 2047] in about 1 run in 20 under load. That still
			// verifies the disk. Anything beyond such a handful means eviction failed.
			if r*100 > n {
				t.Errorf("%d of %d pages cached when the verify read starts: pages %v", r, n, residentIndexes(tmp))
			}
			checked++
		},
	}
	if _, err := copyFile(context.Background(), src, "M1.DNG", []string{dst}, sizeOf(t, src), time.Now(), h); err != nil {
		t.Fatal(err)
	}
	if checked != 1 {
		t.Fatalf("beforeVerify ran %d times", checked)
	}
}

// C3: --checksum compares the destination from the disk: nothing of it cached when
// the read starts.
func TestChecksumReadsDestinationUncached(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {size: 4 << 20}})
	o := opts(t, src)
	o.Checksum = true
	dst := filepath.Join(o.Dest, "2026-10-02 Smith wedding", "M1.DNG")
	card(t, filepath.Dir(dst), map[string]spec{"M1.DNG": {size: 4 << 20}})
	warmCache(t, dst)
	checked := 0
	beforeDiskRead = func(p string) {
		if p != dst {
			return
		}
		checked++
		if r, n, _ := residentPages(p); r*100 > n { // see TestVerifyStartsWithNothingCached: scanners re-read a page or two
			t.Errorf("%d of %d pages cached when --checksum reads the destination", r, n)
		}
	}
	defer func() { beforeDiskRead = nil }()
	if _, err := MakePlan(o); err != nil {
		t.Fatal(err)
	}
	if checked != 1 {
		t.Fatalf("destination read %d times through hashFromDisk", checked)
	}
}

func residentIndexes(p string) []int {
	var out []int
	mapFile(p, func(b []byte) error {
		pg := os.Getpagesize()
		vec := make([]byte, (len(b)+pg-1)/pg)
		syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&vec[0])))
		for i, v := range vec {
			if v&1 != 0 {
				out = append(out, i)
			}
		}
		return nil
	})
	return out
}

// dropCache takes a fully cached file to no pages in the page cache.
func TestDropCacheEvictsWarmFile(t *testing.T) {
	p, _ := randomFile(t, 16<<20)
	warmCache(t, p)
	if r, n, _ := residentPages(p); r != n {
		t.Fatalf("warmCache left %d of %d pages", r, n)
	}
	if err := dropCache(p); err != nil {
		t.Fatal(err)
	}
}

// The temp copies are still written F_NOCACHE (only the card's open dropped it,
// 2026-10-06), so they don't stay cached and the verify read has to come from the
// device. Measured here: 0 of 2048 pages after an F_NOCACHE write, 2048 of 2048 after a
// normal one. (Whether the card is opened F_NOCACHE can't be seen this way: on APFS an
// F_NOCACHE read left every page cached too, and macOS has no F_NOCACHE getter. The
// card bench shows it, as speed.)
func TestTempCopiesStayUncached(t *testing.T) {
	_, b := randomFile(t, 32<<20)
	f, err := createTemp(t.TempDir(), "M1.DNG")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	for off := 0; off < len(b); off += bufSize { // as stream writes it
		if _, err := f.Write(b[off:min(off+bufSize, len(b))]); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	// F_NOCACHE writes kept 0 of 4097 pages in a quiet probe, up to half of some 4 MiB
	// writes under load; a normal write keeps them all.
	if r, n, _ := residentPages(f.Name()); r*4 > n*3 {
		t.Errorf("%d of %d pages of a temp copy cached after writing it: written without F_NOCACHE?", r, n)
	}
}

// evictPasses, as used on the card before a retry, works on a read-only file (so it
// never needs write access), empties the cache of it, and changes nothing.
func TestEvictPassesReadOnlyFile(t *testing.T) {
	p, b := randomFile(t, 16<<20)
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	st0, _ := os.Stat(p)
	warmCache(t, p)
	r, n, err := evictPasses(p)
	if err != nil || r != 0 || n == 0 {
		t.Fatalf("resident %d of %d, err %v", r, n, err)
	}
	st1, _ := os.Stat(p)
	got, _ := os.ReadFile(p)
	if !st1.ModTime().Equal(st0.ModTime()) || st1.Mode() != st0.Mode() || !bytes.Equal(got, b) {
		t.Fatal("evicting the file changed it")
	}
}
