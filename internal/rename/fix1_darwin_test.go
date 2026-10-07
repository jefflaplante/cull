package rename_test

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/rename"
)

// IMPORTANT 2: a locked (uchg) frame is refused before anything moves. Darwin only:
// syscall.Chflags (final review M8: GOOS=linux go vet).
func TestRenameLocked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000), "A2.DNG": frame(2, 1100), "keep/A3.DNG": frame(3, 1200)})
	p := filepath.Join(dir, "A2.DNG")
	if err := chflags(p, true); err != nil {
		t.Skipf("chflags: %v", err)
	}
	defer chflags(p, false)
	before := snapshot(t, dir)
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "A2.DNG") || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked (dry run): %v", err)
	}
	_, err = rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}"})
	chflags(p, false)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked: %v", err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("the tree changed")
	}
}

func chflags(p string, lock bool) error {
	var flags int
	if lock {
		flags = 0x2 // UF_IMMUTABLE
	}
	return syscall.Chflags(p, flags)
}
