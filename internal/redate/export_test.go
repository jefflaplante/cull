package redate

// SetCrashAfterSwap installs the crash seam for the external tests; the returned func
// removes it.
func SetCrashAfterSwap(f func(path string) bool) (restore func()) {
	crashAfterSwap = f
	return func() { crashAfterSwap = nil }
}

// SetTrace records the run's durability steps ("flush <path>", "journal save",
// "journal remove"), in order.
func SetTrace(f func(event string)) (restore func()) {
	trace = f
	return func() { trace = nil }
}
