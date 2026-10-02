//go:build darwin

package offload

import (
	"context"
	"testing"
	"time"
)

// When the verify read starts, none of the copy's pages may be in the page cache:
// otherwise the check is served from RAM and says nothing about the disk. F_NOCACHE
// alone isn't enough under load (half of each 4 MiB write stayed cached in some runs),
// hence the explicit eviction this checks.
func TestVerifyStartsWithNothingCached(t *testing.T) {
	src, _ := randomFile(t, 32<<20)
	dst := t.TempDir()
	checked := 0
	h := hooks{
		afterWrite: func(tmp string) { warmCache(t, tmp) }, // the worst case: every page cached
		beforeVerify: func(tmp string) {
			r, n, err := residentPages(tmp)
			if err != nil {
				t.Fatal(err)
			}
			if r != 0 {
				t.Errorf("%d of %d pages cached when the verify read starts", r, n)
			}
			checked++
		},
	}
	if _, err := copyFile(context.Background(), src, "M1.DNG", []string{dst}, time.Now(), h); err != nil {
		t.Fatal(err)
	}
	if checked != 1 {
		t.Fatalf("beforeVerify ran %d times", checked)
	}
}
