package offload

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// C1: a file the manifest records must still be on every destination to be skipped.
func TestRenameManifestSkipChecksEveryDestination(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}, "DCIM/M2.DNG": {seed: 1}})
	o := opts(t, src)
	o.Rename = "{name}_{n}"
	p, _ := MakePlan(o)
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("first run: %v %+v", err, res)
	}
	// Second run adds a backup: it must receive both files.
	o.Backup = t.TempDir()
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 2 {
		t.Fatalf("second run: %v %+v", err, res)
	}
	for _, n := range []string{"Smith_wedding_1.DNG", "Smith_wedding_2.DNG"} {
		if _, err := os.Stat(filepath.Join(p.Dests[1], n)); err != nil {
			t.Fatalf("backup lacks %s", n)
		}
	}
	// A copy deleted later is copied again, under its recorded name.
	os.Remove(filepath.Join(p.Dests[0], "Smith_wedding_1.DNG"))
	p, _ = MakePlan(o)
	res, err = Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 1 {
		t.Fatalf("third run: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(p.Dests[0], "Smith_wedding_1.DNG")); err != nil {
		t.Fatal("deleted copy not restored under its recorded name")
	}
}

// C2: a source that ends early without an error is a failed copy, not a short one.
func TestShortReadFails(t *testing.T) {
	src, data := randomFile(t, 6<<20)
	a := t.TempDir()
	h := hooks{open: func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data[:3<<20])), nil
	}}
	_, err := copyFile(context.Background(), src, "M1.DNG", []string{a}, int64(len(data)), t0, h)
	if err == nil || !strings.Contains(err.Error(), "short") {
		t.Fatalf("short read accepted: %v", err)
	}
	if ents, _ := os.ReadDir(a); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
}

// C2/I4: --verify compares size too, and names DNGs no manifest line covers.
func TestVerifyChecksSizeAndUnrecorded(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	Run(context.Background(), p, nil)
	os.WriteFile(filepath.Join(p.Dests[0], "X9.DNG"), []byte("finder copy"), 0o644)
	// macOS writes AppleDouble files on exFAT when a frame is opened: not a DNG.
	os.WriteFile(filepath.Join(p.Dests[0], "._M1.DNG"), make([]byte, 4096), 0o644)
	v, err := Verify(context.Background(), p.Dests[0], nil)
	if err != nil || v.OK != 3 || v.Bad != 0 || len(v.Unrecorded) != 1 || v.Unrecorded[0] != "X9.DNG" {
		t.Fatalf("%+v %v", v, err)
	}
	// Truncate a copy: a size mismatch is bad even before hashing.
	f := filepath.Join(p.Dests[0], "M2.DNG")
	os.Truncate(f, 10)
	if v, _ := Verify(context.Background(), p.Dests[0], nil); v.Bad != 1 {
		t.Fatalf("truncated copy: %+v", v)
	}
}

// I1: a pattern with {orig} and no {n} must not crash on a folder that has files.
func TestOrigPatternSecondCard(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M8.DNG": {}})
	o := opts(t, src)
	o.Rename = "{date}_{orig}"
	card(t, filepath.Join(o.Dest, "2026-10-02 Smith wedding"), map[string]spec{"20261002_M7.DNG": {}})
	p, err := MakePlan(o)
	if err != nil || p.Files[0].Name != "20261002_M8.DNG" {
		t.Fatalf("%v %+v", err, p)
	}
}

// I2: a full destination stops the run at once: no retries, no more card reads.
func TestDestinationFullStopsRun(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	opens := map[string]int{}
	p.h.open = func(s string) (io.ReadCloser, error) { opens[filepath.Base(s)]++; return os.Open(s) }
	p.h.write = func(f *os.File, b []byte) (int, error) {
		if strings.Contains(f.Name(), "M2.DNG") {
			return 0, syscall.ENOSPC
		}
		return f.Write(b)
	}
	res, _ := Run(context.Background(), p, nil)
	if res.Safe || res.Copied != 1 || len(res.Failed) != 2 || opens["M2.DNG"] != 1 || opens["M3.DNG"] != 0 {
		t.Fatalf("result %+v opens %v", res, opens)
	}
	if !strings.Contains(strings.Join(res.Failed, ";"), "full") {
		t.Fatalf("failures don't say the destination is full: %v", res.Failed)
	}
}

// I2: a name that already exists is never retried (each retry re-reads the card).
func TestExistsNotRetried(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	os.MkdirAll(p.Dests[0], 0o755)
	os.WriteFile(filepath.Join(p.Dests[0], "M3.DNG"), []byte("appeared after planning"), 0o644)
	opens := 0
	p.h.open = func(s string) (io.ReadCloser, error) {
		if filepath.Base(s) == "M3.DNG" {
			opens++
		}
		return os.Open(s)
	}
	res, _ := Run(context.Background(), p, nil)
	if res.Safe || opens != 1 {
		t.Fatalf("opens %d result %+v", opens, res)
	}
}

// I3: two destinations on one volume need room for both copies.
func TestFreeSpacePerVolume(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {size: 5000}})
	o := opts(t, src)
	o.Backup = t.TempDir() // same volume as Dest
	o.freeSpace = func(string) (uint64, error) { return reserve(5000), nil }
	if _, err := MakePlan(o); err == nil {
		t.Fatal("one copy's room accepted for two copies on the same volume")
	}
	o.freeSpace = func(string) (uint64, error) { return reserve(10000), nil }
	if _, err := MakePlan(o); err != nil {
		t.Fatalf("room for both refused: %v", err)
	}
}

// After --sort (or --move-culled) moves copies into keep/ review/ cull/ culled/,
// --verify still finds and checks them, instead of calling them missing.
func TestVerifyFindsSortedCopies(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	Run(context.Background(), p, nil)
	d := p.Dests[0]
	for name, sub := range map[string]string{"M1.DNG": "keep", "M2.DNG": "review"} {
		os.MkdirAll(filepath.Join(d, sub), 0o755)
		os.Rename(filepath.Join(d, name), filepath.Join(d, sub, name))
	}
	os.WriteFile(filepath.Join(d, "cull", "X.DNG"), nil, 0o644) // a stray, unrecorded, inside a sort folder
	os.MkdirAll(filepath.Join(d, "cull"), 0o755)
	os.WriteFile(filepath.Join(d, "cull", "X.DNG"), []byte("x"), 0o644)
	v, err := Verify(context.Background(), d, nil)
	if err != nil || v.OK != 3 || v.Bad != 0 || len(v.Unrecorded) != 1 || v.Unrecorded[0] != filepath.Join("cull", "X.DNG") {
		t.Fatalf("%+v %v", v, err)
	}
}
