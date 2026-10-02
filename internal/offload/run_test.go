package offload

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() { retryDelays = []time.Duration{time.Millisecond, time.Millisecond} }

// treeHash fingerprints every path, mode, size, mtime and content under root.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		info, _ := d.Info()
		line := fmt.Sprintf("%s %v %d %d", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			line += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func threeFiles(t *testing.T) (string, Options) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {seed: 1, size: 3000}, "DCIM/M2.DNG": {seed: 2, size: 5 << 20}, "DCIM/M3.DNG": {seed: 3}})
	o := opts(t, src)
	o.Backup = t.TempDir()
	return src, o
}

func TestRunCopiesAndIsSafe(t *testing.T) {
	src, o := threeFiles(t)
	before := treeHash(t, src)
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 3 || len(res.Failed) != 0 {
		t.Fatalf("err %v result %+v", err, res)
	}
	for _, d := range p.Dests {
		man, _ := readManifest(d)
		if len(man) != 3 {
			t.Fatalf("%s manifest has %d lines", d, len(man))
		}
		for _, e := range man {
			b, _ := os.ReadFile(filepath.Join(d, e.Name))
			if sum := sha256.Sum256(b); hexOf(sum[:]) != e.SHA256 {
				t.Fatalf("manifest sum wrong for %s", e.Name)
			}
		}
	}
	if treeHash(t, src) != before {
		t.Fatal("the card changed")
	}
}

func TestSourceReadErrorRetriesThenFails(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	calls := 0
	p.h.open = func(s string) (io.ReadCloser, error) {
		if filepath.Base(s) == "M2.DNG" {
			calls++
			return nil, errors.New("input/output error")
		}
		return os.Open(s)
	}
	res, _ := Run(context.Background(), p, nil)
	if res.Safe || len(res.Failed) != 1 || !strings.HasPrefix(res.Failed[0], "M2.DNG") || calls != 3 {
		t.Fatalf("result %+v calls %d", res, calls)
	}
	man, _ := readManifest(p.Dests[0])
	if len(man) != 2 {
		t.Fatalf("manifest %v", man)
	}
}

func TestTransientErrorRecovers(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	calls := 0
	p.h.open = func(s string) (io.ReadCloser, error) {
		if filepath.Base(s) == "M2.DNG" {
			if calls++; calls == 1 {
				return nil, errors.New("input/output error")
			}
		}
		return os.Open(s)
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe || res.Copied != 3 {
		t.Fatalf("err %v result %+v", err, res)
	}
}

func TestWriteErrorFailsFileNotRun(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	p.h.write = func(f *os.File, b []byte) (int, error) {
		if strings.Contains(f.Name(), "M2.DNG") {
			return 0, syscall.ENOSPC
		}
		return f.Write(b)
	}
	res, _ := Run(context.Background(), p, nil)
	if res.Safe || res.Copied != 2 || len(res.Failed) != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestRerunResumes(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	ctx, cancel := context.WithCancel(context.Background())
	p.h.open = func(s string) (io.ReadCloser, error) {
		if filepath.Base(s) == "M2.DNG" {
			cancel()
		}
		return os.Open(s)
	}
	res, err := Run(ctx, p, nil)
	if !errors.Is(err, context.Canceled) || res.Safe || res.Copied != 1 {
		t.Fatalf("first run: err %v result %+v", err, res)
	}
	stale := filepath.Join(p.Dests[0], ".M9.DNG.cull-deadbeef.tmp")
	os.WriteFile(stale, []byte("x"), 0o644)

	p2, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if s := names(p2, true); len(s) != 1 || s[0] != "M1.DNG" {
		t.Fatalf("second plan skips %v", s)
	}
	res, err = Run(context.Background(), p2, nil)
	if err != nil || !res.Safe || res.Copied != 2 || res.Skipped != 1 {
		t.Fatalf("second run: err %v result %+v", err, res)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale temp file left")
	}
}

// A file that was already there by size and mtime (copied by Finder, say) has never
// been checked against the card: it can't count toward "safe to format" unless a
// cull manifest recorded it or --checksum compared it.
func TestUnverifiedSkipsAreNotSafe(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}, "DCIM/M2.DNG": {seed: 1}})
	o := opts(t, src)
	card(t, filepath.Join(o.Dest, "2026-10-02 Smith wedding"), map[string]spec{"M1.DNG": {}})
	p, _ := MakePlan(o)
	res, err := Run(context.Background(), p, nil)
	if err != nil || res.Safe || res.Unverified != 1 {
		t.Fatalf("result %+v err %v", res, err)
	}
	o.Checksum = true
	p, _ = MakePlan(o)
	res, err = Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Unverified != 0 {
		t.Fatalf("with --checksum: result %+v err %v", res, err)
	}
}

func TestVerifyFindsLaterCorruption(t *testing.T) {
	_, o := threeFiles(t)
	p, _ := MakePlan(o)
	if _, err := Run(context.Background(), p, nil); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(p.Dests[0], "M2.DNG")
	b, _ := os.ReadFile(f)
	b[100] ^= 1
	os.WriteFile(f, b, 0o644)
	ok, bad, err := Verify(context.Background(), p.Dests[0], nil)
	if err != nil || ok != 2 || bad != 1 {
		t.Fatalf("ok %d bad %d err %v", ok, bad, err)
	}
	if ok, bad, _ := Verify(context.Background(), p.Dests[1], nil); ok != 3 || bad != 0 {
		t.Fatalf("backup: ok %d bad %d", ok, bad)
	}
}
