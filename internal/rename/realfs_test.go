package rename_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/rename"
)

// On a real exFAT or FAT32 volume (CULL_ADV_DEST=<a folder on it>; skipped otherwise),
// where there are no hard links and macOS keeps extended attributes in "._" AppleDouble
// companions: a rename that swaps two frames' names, interrupted in phase 1, phase 2
// or before the manifest follows, finishes on the next run with each frame's bytes,
// sidecar and tags; --undo puts every name back; companions never pass for temps, and
// no temp is left.
func TestRealFSInterruptedRename(t *testing.T) {
	root := os.Getenv("CULL_ADV_DEST")
	if root == "" {
		t.Skip("set CULL_ADV_DEST to a folder on an exFAT or FAT32 volume")
	}
	for _, stop := range []string{"phase1", "phase2", "manifest"} {
		t.Run(stop, func(t *testing.T) {
			dest, err := os.MkdirTemp(root, "t")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dest)
			c := t.TempDir()
			names := []string{"_DSC0001.DNG", "_DSC0002.DNG", "M3.DNG"}
			fx := map[string]dngtest.Fixture{}
			for i, n := range names {
				fx[n] = frame(int64(i+1), 3000+i)
			}
			data := card(t, c, fx)
			dir := offloadCard(t, c, dest)
			// Named so that "S{n}" swaps the first two: _DSC0001 is S2, _DSC0002 is S1.
			named := map[string]string{"_DSC0001.DNG": "S2.DNG", "_DSC0002.DNG": "S1.DNG", "M3.DNG": "_S3.DNG"}
			es, _ := offload.CurrentManifest(dir)
			for _, e := range es {
				if err := os.Rename(filepath.Join(dir, e.Name), filepath.Join(dir, named[e.Orig])); err != nil {
					t.Fatal(err)
				}
				e.Name = named[e.Orig]
				offload.AppendManifest(dir, e)
			}
			// Tags make "._" companions on exFAT and FAT.
			for _, n := range named {
				if err := unix.Setxattr(filepath.Join(dir, n), "com.apple.metadata:_kMDItemUserTags", []byte("tag"), 0); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(dir, strings.TrimSuffix(n, ".DNG")+".xmp"), []byte("<x:xmpmeta/>"+n), 0o644)
			}
			restore := rename.SetCrash(func(step string, i int) bool { return step == stop && (i == 1 || stop == "manifest") })
			_, err = rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
			restore()
			if !errors.Is(err, rename.ErrCrash) {
				t.Fatalf("err %v", err)
			}
			ents, _ := os.ReadDir(dir)
			var seen []string
			for _, e := range ents {
				seen = append(seen, e.Name())
			}
			t.Logf("stopped at %s: %v", stop, seen)
			for tmp := range offload.RenameTemps(dir) {
				if strings.HasPrefix(filepath.Base(tmp), "._") {
					t.Fatalf("a companion taken for a temp: %s", tmp)
				}
			}
			runRename(t, rename.Options{Dir: dir, Pattern: "S{n}", Reorder: true})
			check := func(where map[string]string) {
				t.Helper()
				for orig, n := range where {
					p := filepath.Join(dir, n)
					if sum(read(t, p)) != sum(data[orig]) {
						t.Errorf("%s isn't %s", n, orig)
					}
					if got := string(read(t, strings.TrimSuffix(p, ".DNG")+".xmp")); got != "<x:xmpmeta/>"+named[orig] {
						t.Errorf("%s's sidecar: %q", n, got)
					}
					if v, err := getxattr(p); err != nil || v != "tag" {
						t.Errorf("%s lost its tag: %q %v", n, v, err)
					}
				}
				if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 3 {
					t.Errorf("verify %+v %v", v, err)
				}
				for _, e := range func() []os.DirEntry { e, _ := os.ReadDir(dir); return e }() {
					if strings.Contains(e.Name(), ".cull-rename-") && !strings.HasPrefix(e.Name(), "._") {
						t.Errorf("temp left: %s", e.Name())
					}
				}
			}
			check(map[string]string{"_DSC0001.DNG": "S1.DNG", "_DSC0002.DNG": "S2.DNG", "M3.DNG": "S3.DNG"})
			runRename(t, rename.Options{Dir: dir, Undo: true})
			check(named)
			if j, _ := journal.LoadRename(dir); j != nil {
				t.Fatal("journal left after undo")
			}
		})
	}
}

func getxattr(p string) (string, error) {
	buf := make([]byte, 64)
	n, err := unix.Getxattr(p, "com.apple.metadata:_kMDItemUserTags", buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
