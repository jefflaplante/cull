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
// a temp with xattrs gets an AppleDouble "._" companion ("._.cull-redate-<hex>.<name>"). An interrupted replacement is restored on the next run,
// and the companion is never taken for a temp.
func TestRealFSInterruptedWithAppleDouble(t *testing.T) { realFSInterrupted(t, "M1.DNG", "M2.DNG") }

// The same for frames whose names start with "_" (Pentax _IGP, Nikon/Sony _DSC, Canon
// _MG_): their temps must never look like AppleDouble companions.
func TestRealFSInterruptedUnderscore(t *testing.T) {
	realFSInterrupted(t, "_IGP0001.DNG", "_IGP0002.DNG")
}

func realFSInterrupted(t *testing.T, names ...string) {
	t.Helper()
	root := os.Getenv("CULL_ADV_DEST")
	if root == "" {
		t.Skip("set CULL_ADV_DEST to a folder on an exFAT or FAT32 volume")
	}
	dest, err := os.MkdirTemp(root, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dest)
	fx := map[string]dngtest.Fixture{}
	for i, n := range names {
		fx[n] = dated(int64(i+1), 4000)
	}
	c := fakeCard(t, fx)
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := offload.Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("offload %v %+v", err, res)
	}
	dir := p.Dests[0]
	m1 := filepath.Join(dir, names[0])
	if err := unix.Setxattr(m1, "com.apple.metadata:_kMDItemUserTags", []byte("tag"), 0); err != nil {
		t.Fatal(err)
	}
	restore := offload.SetRenameHook(func(old, new string) error {
		if filepath.Base(new) == names[0] {
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
	temps := offload.RedateTemps(dir)
	for tmp := range temps {
		if filepath.Base(tmp)[:2] == "._" {
			t.Fatalf("AppleDouble companion taken for a temp: %s", tmp)
		}
	}
	if len(temps) != 1 {
		t.Fatalf("run 1 left temps %v", temps)
	}
	res, n, err = runNotes(t, dir)
	if err != nil || len(res.Orphans) != 0 || res.Refused != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if _, err := os.Stat(m1); err != nil {
		t.Fatalf("%s not restored", names[0])
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal incomplete")
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != len(names) {
		t.Fatalf("verify %+v %v", v, err)
	}
}
