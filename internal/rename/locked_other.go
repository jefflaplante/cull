//go:build !darwin

package rename

import "os"

// lockedFlag: only macOS's file flags are checked; elsewhere the move itself fails.
func lockedFlag(os.FileInfo) string { return "" }
