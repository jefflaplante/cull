//go:build darwin

package offload

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func birthtime(t *testing.T, p string) time.Time {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return time.Unix(st.Btim.Unix())
}
