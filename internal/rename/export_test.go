package rename

// ErrCrash is what Run returns when the SetCrash seam stops it.
var ErrCrash = errCrash

// SetCrash installs the crash seam for the external tests; the returned func removes it.
func SetCrash(f func(step string, i int) bool) (restore func()) {
	crash = f
	return func() { crash = nil }
}

// SetTrace records the run's moves ("move <from> -> <to>") and durability steps
// ("flush <path>", "journal phase=… report=… complete=…", "journal remove",
// "report save", "labels append", "manifest append"), in order.
func SetTrace(f func(event string)) (restore func()) {
	trace = f
	return func() { trace = nil }
}
