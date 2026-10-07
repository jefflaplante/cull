//go:build !darwin

package offload

import (
	"testing"
	"time"
)

// birthtime isn't read off macOS; TestSetFileTimes checks it only on darwin.
func birthtime(t *testing.T, p string) time.Time { return time.Time{} }
