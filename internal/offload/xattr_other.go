//go:build !darwin && !linux

package offload

import "os"

// copyXattrs is a no-op where cull doesn't know the extended-attribute calls.
func copyXattrs(from, to *os.File) []error { return nil }
