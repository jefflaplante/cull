// Package dcf is camera order: the order a camera wrote its files in, read from the
// DCF (Design rule for Camera File system) names it gives them.
//
// A DCF file name is 4 free characters and a 4-digit file counter ("M1102767.DNG",
// "IMG_1234.JPG"), in a DCF folder of a 3-digit number and 5 free characters
// ("100LEICA"); the folder number goes up when the counter wraps past 9999. The
// counter, not the name, is the order: a Leica M11-P numbers its "M…" frames and its
// "L…" (Content Credentials) frames from one counter (M1102771 then L1002772), so by
// name every L frame would sort before every M frame. Capture times can't stand in:
// a camera clock that wasn't running gives many frames one time.
package dcf

import (
	"path/filepath"
	"strings"
)

// Name is a file's place in camera order.
type Name struct {
	Folder int    // DCF folder number, 100–999; 0 when unknown
	Base   string // the file name, with its extension
}

// Of is the Name of path p: its base name, with the folder number when p's parent
// folder has the DCF shape ("100LEICA/M1103127.DNG", a card path). A bare name, or
// one in a folder of another shape (a shoot folder), has no known folder.
func Of(p string) Name {
	n := Name{Base: filepath.Base(p)}
	if p == "" {
		n.Base = ""
	}
	if dir := filepath.Dir(p); dir != "." && dir != p {
		n.Folder = folderNumber(filepath.Base(dir))
	}
	return n
}

// free reports whether c is a DCF free character: A–Z, 0–9 or _ (either case is
// accepted: file systems and cameras differ).
func free(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func digits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, len(s) > 0
}

// folderNumber is a DCF folder's number: 8 characters, a number 100–999 then 5 free
// characters. 0 for any other name.
func folderNumber(dir string) int {
	if len(dir) != 8 {
		return 0
	}
	n, ok := digits(dir[:3])
	if !ok || n < 100 {
		return 0
	}
	for i := 3; i < 8; i++ {
		if !free(dir[i]) {
			return 0
		}
	}
	return n
}

// counter is a DCF file name's counter: the stem (the name without its extension)
// is exactly 8 characters, 4 free characters then 4 digits. -1 for any other name.
func counter(base string) int {
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if len(stem) != 8 {
		return -1
	}
	for i := 0; i < 4; i++ {
		if !free(stem[i]) {
			return -1
		}
	}
	n, ok := digits(stem[4:])
	if !ok {
		return -1
	}
	return n
}

// IsName reports whether base has the DCF file-name shape (see counter): such a name
// is ordered by its last 4 digits, not as text.
func IsName(base string) bool { return counter(base) >= 0 }

// Unify clears every name's folder unless all the DCF names have one. Folder numbers
// order frames only when every frame's is known: a frame of unknown folder can't be
// placed against a wrap, and "unknown matches any folder" isn't a transitive order.
// Callers ordering a set unify its names first.
func Unify(names []Name) {
	all := true
	for _, n := range names {
		if counter(n.Base) >= 0 && n.Folder == 0 {
			all = false
		}
	}
	if all {
		return
	}
	for i := range names {
		names[i].Folder = 0
	}
}

// Compare orders a and b in camera order, -1, 0 or +1. DCF names come first: by
// folder number (0, unknown, before any; Unify makes a set all known or all
// unknown), then counter. Every other name follows, by name. Ties go to the name
// ignoring case, then the exact name. It is a total order on any names; 0 means the
// same DCF name in the same folder, or the same other name.
func Compare(a, b Name) int {
	ca, cb := counter(a.Base), counter(b.Base)
	switch {
	case (ca >= 0) != (cb >= 0):
		if ca >= 0 {
			return -1
		}
		return 1
	case ca >= 0:
		if c := cmpInt(a.Folder, b.Folder); c != 0 {
			return c
		}
		if c := cmpInt(ca, cb); c != 0 {
			return c
		}
	}
	if c := strings.Compare(strings.ToLower(a.Base), strings.ToLower(b.Base)); c != 0 {
		return c
	}
	return strings.Compare(a.Base, b.Base)
}

// Less is Compare(a, b) < 0.
func Less(a, b Name) bool { return Compare(a, b) < 0 }

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
