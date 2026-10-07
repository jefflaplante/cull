package offload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
)

func replaceFixture(t *testing.T) (path string, b []byte, ps []dng.Patch) {
	t.Helper()
	return replaceFixtureNamed(t, "M7.DNG")
}

func replaceFixtureNamed(t *testing.T, name string) (path string, b []byte, ps []dng.Patch) {
	t.Helper()
	dir := t.TempDir()
	b = dngtest.Build(t, dated(7, 9<<20))
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, t0, t0)
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), setTarget)
	if err != nil || len(ps) == 0 {
		t.Fatalf("patches %v %v", ps, err)
	}
	return path, b, ps
}

func temps(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	r, _ := filepath.Glob(filepath.Join(dir, RedateTempPrefix+"*"))
	return append(m, r...)
}

// The file becomes exactly its old bytes with the patches, with its times set; both
// hashes are returned and beforeSwap sees them before the rename.
func TestReplacePatched(t *testing.T) {
	path, b, ps := replaceFixture(t)
	var seen [2][32]byte
	r, err := ReplacePatched(context.Background(), path, ps, hexOf(sha(b)), setTarget, func(orig, want [32]byte) error {
		seen = [2][32]byte{orig, want}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, b) {
			t.Error("replaced before beforeSwap")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := applyAll(t, b)
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, want) || r.Orig != sha256.Sum256(b) || r.Want != sha256.Sum256(want) || seen != [2][32]byte{r.Orig, r.Want} {
		t.Fatal("not replaced with exactly the patches")
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(setTarget) {
		t.Fatalf("mtime %v", st.ModTime())
	}
	if l := temps(t, filepath.Dir(path)); len(l) != 0 {
		t.Fatalf("temps left: %v", l)
	}
}

// Refusals and failures leave the original untouched and no temp behind.
func TestReplacePatchedRefuses(t *testing.T) {
	check := func(t *testing.T, path string, b []byte) {
		t.Helper()
		if got, _ := os.ReadFile(path); !bytes.Equal(got, b) {
			t.Fatal("original changed")
		}
		if st, _ := os.Stat(path); !st.ModTime().Equal(t0) {
			t.Fatal("original's mtime changed")
		}
		if l := temps(t, filepath.Dir(path)); len(l) != 0 {
			t.Fatalf("temps left: %v", l)
		}
	}
	t.Run("checksum", func(t *testing.T) {
		path, b, ps := replaceFixture(t)
		_, err := ReplacePatched(context.Background(), path, ps, strings.Repeat("0", 64), setTarget, nil)
		if !errors.Is(err, ErrChanged) {
			t.Fatalf("err %v", err)
		}
		check(t, path, b)
	})
	t.Run("proof", func(t *testing.T) {
		path, b, ps := replaceFixture(t)
		beforeDiskRead = func(p string) { // a stray byte on the way to the disk
			f, _ := os.OpenFile(p, os.O_WRONLY, 0)
			f.WriteAt([]byte{0}, 5<<20)
			f.Close()
		}
		defer func() { beforeDiskRead = nil }()
		_, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil)
		if err == nil || !strings.Contains(err.Error(), "prove") {
			t.Fatalf("err %v", err)
		}
		check(t, path, b)
	})
	t.Run("beforeSwap", func(t *testing.T) {
		path, b, ps := replaceFixture(t)
		_, err := ReplacePatched(context.Background(), path, ps, "", setTarget, func(_, _ [32]byte) error { return errors.New("journal") })
		if err == nil || err.Error() != "journal" {
			t.Fatalf("err %v", err)
		}
		check(t, path, b)
	})
	t.Run("stale patches", func(t *testing.T) {
		path, b, ps := replaceFixture(t)
		ps[0].Old = []byte(strings.Repeat("x", len(ps[0].Old)))
		if _, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil); err == nil {
			t.Fatal("patches for other bytes accepted")
		}
		check(t, path, b)
	})
	t.Run("hard link", func(t *testing.T) {
		path, b, ps := replaceFixture(t)
		if err := os.Link(path, path+".other"); err != nil {
			t.Skip(err)
		}
		_, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil)
		if err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("err %v", err)
		}
		check(t, path, b)
	})
}

func sha(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// On a network share F_FULLFSYNC isn't supported: the temp and its folder are flushed
// with fsync instead, as offload's destination flush does.
func TestReplacePatchedNetworkShare(t *testing.T) {
	path, b, ps := replaceFixture(t)
	full := 0
	fullSyncFn = func(*os.File) error { full++; return syscall.ENOTSUP }
	defer func() { fullSyncFn = fullSync }()
	if _, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, applyAll(t, b)) || full != 2 {
		t.Fatalf("full syncs tried %d", full)
	}
}

// Any other flush failure of the temp stops before the swap.
func TestReplacePatchedFlushFails(t *testing.T) {
	path, b, ps := replaceFixture(t)
	fullSyncFn = func(*os.File) error { return syscall.EIO }
	defer func() { fullSyncFn = fullSync }()
	if _, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil); !errors.Is(err, syscall.EIO) {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, b) {
		t.Fatal("replaced after a failed flush")
	}
	if l := temps(t, filepath.Dir(path)); len(l) != 0 {
		t.Fatalf("temps left: %v", l)
	}
}

// The journal record (beforeSwap) is written before the temp's F_FULLFSYNC, so that
// flush carries it to the media before the swap; the folder is flushed after.
func TestReplacePatchedJournalBeforeFullSync(t *testing.T) {
	path, _, ps := replaceFixture(t)
	var order []string
	fullSyncFn = func(f *os.File) error {
		order = append(order, "full "+filepath.Base(f.Name()))
		return fullSync(f)
	}
	defer func() { fullSyncFn = fullSync }()
	if _, err := ReplacePatched(context.Background(), path, ps, "", setTarget, func(_, _ [32]byte) error {
		order = append(order, "journal")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "journal" || !strings.HasPrefix(order[1], "full "+RedateTempPrefix) || order[2] != "full "+filepath.Base(filepath.Dir(path)) {
		t.Fatalf("order %q", order)
	}
}

// A redate temp is never removed once its target name is gone: it may be the only
// copy (a rename-over that deletes first, interrupted).
func TestReplacePatchedKeepsTempWithoutTarget(t *testing.T) {
	path, _, ps := replaceFixture(t)
	_, err := ReplacePatched(context.Background(), path, ps, "", setTarget, func(_, _ [32]byte) error {
		return os.Remove(path) // the original's name is gone before the swap
	})
	if err == nil {
		t.Fatal("no error")
	}
	if l, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".cull-redate-*.M7.DNG")); len(l) != 1 {
		t.Fatalf("temp not kept: %v", l)
	}
}

// Offload's sweep of its own stale temps leaves redate's alone.
func TestRemoveStaleTempsSparesRedate(t *testing.T) {
	dir := t.TempDir()
	mine, theirs := filepath.Join(dir, ".A.DNG.cull-01020304.tmp"), filepath.Join(dir, ".cull-redate-01020304.A.DNG")
	os.WriteFile(mine, nil, 0o644)
	os.WriteFile(theirs, nil, 0o644)
	RemoveStaleTemps(dir)
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatal("offload temp kept")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("redate temp removed")
	}
}

// Folder names with glob characters: both sweeps find their temps by name.
func TestTempsInGlobFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-02 trip [day 1] *?")
	os.Mkdir(dir, 0o755)
	mine, theirs := filepath.Join(dir, ".A.DNG.cull-01020304.tmp"), filepath.Join(dir, ".cull-redate-01020304.A.DNG")
	os.WriteFile(mine, nil, 0o644)
	os.WriteFile(theirs, nil, 0o644)
	if got := RedateTemps(dir); got[theirs] != filepath.Join(dir, "A.DNG") || len(got) != 1 {
		t.Fatalf("redate temps %v", got)
	}
	RemoveStaleTemps(dir)
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatal("offload temp kept")
	}
}

// The swap reports that it kept its temp because the original's name is gone.
func TestReplacePatchedTempKept(t *testing.T) {
	path, _, ps := replaceFixture(t)
	restore := SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	defer restore()
	r, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil)
	if err == nil || r.Swapped || !r.TempKept {
		t.Fatalf("%+v %v", r, err)
	}
}

// macOS's AppleDouble companions ("._" + name) on exFAT/FAT are never temps.
func TestTempsSkipAppleDouble(t *testing.T) {
	dir := t.TempDir()
	ad1, ad2 := filepath.Join(dir, "._.cull-redate-01020304.A.DNG"), filepath.Join(dir, "._.A.DNG.cull-01020304.tmp")
	os.WriteFile(ad1, nil, 0o644)
	os.WriteFile(ad2, nil, 0o644)
	if got := RedateTemps(dir); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	RemoveStaleTemps(dir)
	if _, err := os.Stat(ad2); err != nil {
		t.Fatal("companion removed")
	}
}

// The original vanishing while it is read leaves the temp kept but unproven.
func TestReplacePatchedVanishesWhileRead(t *testing.T) {
	path, _, ps := replaceFixture(t)
	restore := SetAfterStreamHook(func(p string) { os.Remove(p) })
	defer restore()
	r, err := ReplacePatched(context.Background(), path, ps, strings.Repeat("0", 64), setTarget, func(_, _ [32]byte) error {
		t.Fatal("beforeSwap called")
		return nil
	})
	if err == nil || !r.TempKept || r.Proven || r.Temp == "" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Redate's temps are ".cull-redate-<8 hex>.<name>". A frame named "_IGP0001.DNG" (Pentax;
// Nikon and Sony "_DSC", Canon "_MG_") once got "._IGP0001.DNG.cull-<hex>.redate",
// which reads as an AppleDouble companion: skipped by recovery and status, and deleted
// by dot_clean or find -name '._*' -delete, though it may be the frame's only copy.
func TestRedateTempNeverLooksAppleDouble(t *testing.T) {
	for _, name := range []string{"_IGP0001.DNG", "M7.DNG", "_.DNG"} {
		path, _, ps := replaceFixtureNamed(t, name)
		restore := SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
		r, err := ReplacePatched(context.Background(), path, ps, "", setTarget, nil)
		restore()
		if err == nil || !r.TempKept || !r.Proven {
			t.Fatalf("%s: %+v %v", name, r, err)
		}
		base := filepath.Base(r.Temp)
		if strings.HasPrefix(base, "._") || !strings.HasPrefix(base, RedateTempPrefix) || !strings.HasSuffix(base, "."+name) {
			t.Fatalf("%s: temp named %s", name, base)
		}
		if got := RedateTemps(filepath.Dir(path)); len(got) != 1 || got[r.Temp] != path {
			t.Fatalf("%s: RedateTemps %v", name, got)
		}
	}
}

// Offload's sweep takes the crash temp of an "_" frame ("._IGP0001.DNG.cull-<hex>.tmp"),
// never an AppleDouble companion ("._." + a hidden temp's name), and never a redate temp.
func TestRemoveStaleTempsUnderscoreFrames(t *testing.T) {
	dir := t.TempDir()
	swept := []string{"._IGP0001.DNG.cull-01020304.tmp", "._DSC0001.DNG.cull-0a0b0c0d.tmp", ".M1.DNG.cull-01020304.tmp"}
	kept := []string{
		"._.M1.DNG.cull-01020304.tmp",          // the companion of .M1.DNG.cull-….tmp
		"._._IGP0001.DNG.cull-01020304.tmp",    // the companion of ._IGP0001.DNG.cull-….tmp
		".cull-redate-01020304._IGP0001.DNG",   // redate's: may be the only copy
		"._.cull-redate-01020304._IGP0001.DNG", // its companion
		"_IGP0001.DNG", "._IGP0001.DNG",        // a frame and its companion
	}
	for _, n := range append(append([]string{}, swept...), kept...) {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	RemoveStaleTemps(dir)
	for _, n := range swept {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s not swept", n)
		}
	}
	for _, n := range kept {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s removed", n)
		}
	}
}

// RedateTemps maps each temp back to its frame, "_" names included, and never takes an
// AppleDouble companion ("._.cull-redate-…") or an offload temp.
func TestRedateTempsNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		".cull-redate-01020304._IGP0001.DNG", ".cull-redate-0a0b0c0d.M1.DNG",
		"._.cull-redate-01020304._IGP0001.DNG", "._.cull-redate-0a0b0c0d.M1.DNG",
		"._IGP0001.DNG.cull-01020304.tmp", ".cull-redate-xyz.M1.DNG", ".cull-redate-01020304.", ".cull-redate-01020304..",
	} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	got := RedateTemps(dir)
	want := map[string]string{
		filepath.Join(dir, ".cull-redate-01020304._IGP0001.DNG"): filepath.Join(dir, "_IGP0001.DNG"),
		filepath.Join(dir, ".cull-redate-0a0b0c0d.M1.DNG"):       filepath.Join(dir, "M1.DNG"),
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s → %q, want %q (all %v)", k, got[k], v, got)
		}
	}
}
