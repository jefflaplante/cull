//go:build !darwin

package offload

import (
	"os"
	"time"
)

// noCache is a no-op off macOS: the verify read may then be served from the page
// cache, so it checks less than on macOS.
func noCache(*os.File) error { return nil }

func fullSync(f *os.File) error { return f.Sync() }

func plainSync(f *os.File) error { return f.Sync() }

// residentPages can't be measured here; evict is a no-op.
func residentPages(string) (resident, pages int, err error) { return 0, 0, nil }

func evict(string) error { return nil }

// setCreationTime is a no-op off macOS: there is no portable creation time to set.
func setCreationTime(string, time.Time) error { return nil }
