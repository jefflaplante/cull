package rename

import (
	"os"
	"syscall"
)

// lockedFlag says why a file can't be renamed: an immutable flag (Finder's Locked,
// chflags uchg or schg); "" if none.
func lockedFlag(st os.FileInfo) string {
	if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Flags&(0x2|0x20000) != 0 { // UF_IMMUTABLE, SF_IMMUTABLE
		return "it is locked"
	}
	return ""
}
