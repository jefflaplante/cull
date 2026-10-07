//go:build darwin

package offload

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// noCache stops the kernel keeping f's pages in the page cache (fcntl F_NOCACHE): a
// copy written this way can only be read back from the device, which is what makes
// the verify read mean something. Measured: 0 of 4097 pages of a 64 MB write stayed
// resident, against all of them for a normal write.
func noCache(f *os.File) error {
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 1)
	if e != 0 {
		return e
	}
	return nil
}

// fullSync asks the drive to empty its own write cache to the media (F_FULLFSYNC);
// fsync(2) on macOS stops at the drive. Done once per destination before "safe to
// format", not per file: it flushes the whole device and costs accordingly.
func fullSync(f *os.File) error {
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0)
	if e != 0 {
		return e
	}
	return nil
}

// plainSync is fsync(2). Go's os.File.Sync is F_FULLFSYNC on darwin, too costly per file.
func plainSync(f *os.File) error { return syscall.Fsync(int(f.Fd())) }

// mapFile maps p read-only for fn; an empty file has nothing to map.
func mapFile(p string, fn func(b []byte) error) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	defer syscall.Munmap(b)
	return fn(b)
}

// residentPages counts p's pages in the page cache (mincore).
func residentPages(p string) (resident, pages int, err error) {
	err = mapFile(p, func(b []byte) error {
		pg := os.Getpagesize()
		pages = (len(b) + pg - 1) / pg
		vec := make([]byte, pages)
		if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
			return e
		}
		for _, v := range vec {
			resident += int(v & 1)
		}
		return nil
	})
	return resident, pages, err
}

// evict drops p's clean pages from the page cache (msync MS_INVALIDATE on a mapping).
func evict(p string) error {
	return mapFile(p, func(b []byte) error {
		if _, _, e := syscall.Syscall(syscall.SYS_MSYNC, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), syscall.MS_INVALIDATE); e != 0 {
			return e
		}
		return nil
	})
}

// setCreationTime sets p's creation (birth) time to t (setattrlist ATTR_CMN_CRTIME),
// which Finder and Capture One show as "Created". Not every filesystem keeps one.
func setCreationTime(p string, t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&ts)), unsafe.Sizeof(ts))
	return unix.Setattrlist(p, &al, buf, unix.FSOPT_NOFOLLOW)
}
