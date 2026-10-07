//go:build darwin || linux

package offload

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// copyXattrs copies every extended attribute of from onto to (Finder tags, comments):
// a swapped-in file is a new inode. Best effort: each attribute that can't be read or
// written is returned as an error naming it (a filesystem without xattrs gives none).
func copyXattrs(from, to *os.File) []error {
	names, err := listXattrs(int(from.Fd()))
	if err != nil {
		if unsupportedXattr(err) {
			return nil
		}
		return []error{fmt.Errorf("list extended attributes: %w", err)}
	}
	var errs []error
	for _, n := range names {
		v, err := getXattr(int(from.Fd()), n)
		if err == nil {
			err = unix.Fsetxattr(int(to.Fd()), n, v, 0)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("extended attribute %s: %w", n, err))
		}
	}
	return errs
}

func unsupportedXattr(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

func listXattrs(fd int) ([]string, error) {
	for {
		n, err := unix.Flistxattr(fd, nil)
		if err != nil || n == 0 {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = unix.Flistxattr(fd, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue // grew meanwhile
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, b := range bytes.Split(buf[:n], []byte{0}) {
			if len(b) > 0 {
				names = append(names, string(b))
			}
		}
		return names, nil
	}
}

func getXattr(fd int, name string) ([]byte, error) {
	for {
		n, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if n == 0 {
			return buf, nil
		}
		n, err = unix.Fgetxattr(fd, name, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}
