package redate

// SetCrashAfterSwap installs the crash seam for the external tests; the returned func
// removes it.
func SetCrashAfterSwap(f func(path string) bool) (restore func()) {
	crashAfterSwap = f
	return func() { crashAfterSwap = nil }
}
