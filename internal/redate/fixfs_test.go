package redate_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
)

// On a real exFAT or FAT32 volume (CULL_ADV_DEST=<a folder on it>; skipped otherwise):
// a temp with xattrs gets an AppleDouble "._" companion, which also starts with "." and
// ends ".cull-<hex>.redate". An interrupted replacement is restored on the next run,
// and the companion is never taken for a temp.
func TestRealFSInterruptedWithAppleDouble(t *testing.T) {
	root := os.Getenv("CULL_ADV_DEST")
	if root == "" {
		t.Skip("set CULL_ADV_DEST to a folder on an exFAT or FAT32 volume")
	}
	dest, err := os.MkdirTemp(root, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dest)
	c := fakeCard(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := offload.Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("offload %v %+v", err, res)
	}
	dir := p.Dests[0]
	m1 := filepath.Join(dir, "M1.DNG")
	if err := unix.Setxattr(m1, "com.apple.metadata:_kMDItemUserTags", []byte("tag"), 0); err != nil {
		t.Fatal(err)
	}
	restore := offload.SetRenameHook(func(old, new string) error {
		if filepath.Base(new) == "M1.DNG" {
			os.Remove(new)
			return syscall.EIO
		}
		return os.Rename(old, new)
	})
	res, n, _ := runNotes(t, dir)
	restore()
	if res.Interrupted != 1 {
		t.Fatalf("run 1: %+v\n%s", res, n.all())
	}
	for tmp := range offload.RedateTemps(dir) {
		if filepath.Base(tmp)[:2] == "._" {
			t.Fatalf("AppleDouble companion taken for a temp: %s", tmp)
		}
	}
	res, n, err = runNotes(t, dir)
	if err != nil || len(res.Orphans) != 0 || res.Refused != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if _, err := os.Stat(m1); err != nil {
		t.Fatal("M1.DNG not restored")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal incomplete")
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 2 {
		t.Fatalf("verify %+v %v", v, err)
	}
}
