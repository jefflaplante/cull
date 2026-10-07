package offload

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)

type spec struct {
	size  int
	mtime time.Time
	seed  byte
}

// card writes synthetic files under root (paths relative, "/"-separated). They are not
// DNGs, so capture time falls back to the mtime.
func card(t *testing.T, root string, files map[string]spec) {
	t.Helper()
	for rel, s := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content(s), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := s.mtime
		if mt.IsZero() {
			mt = t0
		}
		os.Chtimes(p, mt, mt)
	}
}

func content(s spec) []byte {
	n := s.size
	if n == 0 {
		n = 1000
	}
	b := bytes.Repeat([]byte{s.seed + 1}, n)
	return b
}

func opts(t *testing.T, src string) Options {
	return Options{Sources: []string{src}, Dest: t.TempDir(), Name: "Smith wedding",
		freeSpace: func(string) (uint64, error) { return 1 << 50, nil }}
}

func names(p *Plan, skipped bool) []string {
	var out []string
	for _, f := range p.Files {
		if (f.Skip != "") == skipped {
			out = append(out, f.Name)
		}
	}
	return out
}

func TestPlanWalksCardHygienically(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{
		"DCIM/100LEICA/M1.DNG":   {},
		"DCIM/101LEICA/m3.dng":   {},
		"DCIM/100LEICA/._M1.DNG": {},
		".Trashes/501/x.DNG":     {},
		".fseventsd/f":           {},
		"DCIM/100LEICA/M2.jpg":   {},
	})
	elsewhere := filepath.Join(t.TempDir(), "L.DNG")
	os.WriteFile(elsewhere, []byte("x"), 0o644)
	os.Symlink(elsewhere, filepath.Join(src, "DCIM", "100LEICA", "L.DNG"))
	p, err := MakePlan(opts(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(p, false), ","); got != "M1.DNG,m3.dng" {
		t.Fatalf("files %s", got)
	}
}

func TestPlanFolderDate(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{
		"DCIM/M1.DNG": {mtime: t0.Add(48 * time.Hour)},
		"DCIM/M2.DNG": {mtime: t0},
	})
	p, err := MakePlan(opts(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if p.Folder != "2026-10-02 Smith wedding" || !strings.Contains(p.Dated, "M2.DNG") {
		t.Fatalf("folder %q dated %q", p.Folder, p.Dated)
	}
	o := opts(t, src)
	o.Date, o.Name = "2026-09-30", ""
	if p, err = MakePlan(o); err != nil || p.Folder != "2026-09-30" || p.Dated != "--date" {
		t.Fatalf("with --date: %v %+v", err, p)
	}
	o.Date = "30/09/2026"
	if _, err := MakePlan(o); err == nil {
		t.Fatal("bad --date accepted")
	}
}

func TestPlanSkipsAlreadyCopied(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}, "DCIM/M2.DNG": {}})
	o := opts(t, src)
	folder := filepath.Join(o.Dest, "2026-10-02 Smith wedding")
	card(t, folder, map[string]spec{"M1.DNG": {mtime: t0.Add(time.Second)}})
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if s := names(p, true); len(s) != 1 || s[0] != "M1.DNG" || p.Bytes != 1000 {
		t.Fatalf("skipped %v bytes %d", s, p.Bytes)
	}
}

func TestPlanRefusesClash(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}})
	o := opts(t, src)
	folder := filepath.Join(o.Dest, "2026-10-02 Smith wedding")
	card(t, folder, map[string]spec{"M1.DNG": {mtime: t0.Add(3 * time.Second)}})
	if _, err := MakePlan(o); err == nil || !strings.Contains(err.Error(), "--rename") {
		t.Fatalf("clash not refused: %v", err)
	}

	src2 := t.TempDir()
	card(t, src2, map[string]spec{"DCIM/M1.DNG": {seed: 2}})
	o = opts(t, src)
	o.Sources = append(o.Sources, src2)
	_, err := MakePlan(o)
	if err == nil || !strings.Contains(err.Error(), "M1.DNG") {
		t.Fatalf("same name on two cards not refused: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(o.Dest, "2026-10-02 Smith wedding")); serr == nil {
		t.Fatal("planning created the shoot folder")
	}
}

func TestPlanChecksumMode(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}, "DCIM/M2.DNG": {}})
	o := opts(t, src)
	o.Checksum = true
	folder := filepath.Join(o.Dest, "2026-10-02 Smith wedding")
	// Same content, mtime far off: --checksum still recognizes it.
	card(t, folder, map[string]spec{"M1.DNG": {mtime: t0.Add(time.Hour)}})
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if s := names(p, true); len(s) != 1 || s[0] != "M1.DNG" {
		t.Fatalf("skipped %v", s)
	}
	// Same size and mtime, different bytes: --checksum refuses where the quick check would skip.
	card(t, folder, map[string]spec{"M2.DNG": {seed: 9}})
	if _, err := MakePlan(o); err == nil {
		t.Fatal("different content with matching size and mtime accepted under --checksum")
	}
}

func TestRenameCounterContinues(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M2.DNG": {}, "DCIM/M1.DNG": {mtime: t0.Add(time.Minute)}})
	o := opts(t, src)
	o.Rename = "{date}_{name}_{n:4}"
	card(t, filepath.Join(o.Dest, "2026-10-02 Smith wedding"), map[string]spec{"20261002_Smith_wedding_0007.DNG": {seed: 5}})
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	// Numbered in camera order, not capture order.
	if got := strings.Join(names(p, false), ","); got != "20261002_Smith_wedding_0008.DNG,20261002_Smith_wedding_0009.DNG" {
		t.Fatalf("names %s", got)
	}
	if filepath.Base(p.Files[0].Src) != "M1.DNG" {
		t.Fatalf("first is %s", p.Files[0].Src)
	}
}

func TestRenameSkipsManifestEntries(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}, "DCIM/M2.DNG": {}})
	o := opts(t, src)
	o.Rename = "{name}_{n}"
	folder := filepath.Join(o.Dest, "2026-10-02 Smith wedding")
	card(t, folder, map[string]spec{"Smith_wedding_1.DNG": {}})
	sum := sha256.Sum256(content(spec{}))
	line, _ := json.Marshal(Entry{Src: "/Volumes/X/DCIM/M1.DNG", Orig: "M1.DNG", Name: "Smith_wedding_1.DNG", Size: 1000, ModTime: t0, SHA256: string(hexOf(sum[:]))})
	os.WriteFile(filepath.Join(folder, ManifestName), append(line, '\n'), 0o644)
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if s := names(p, true); len(s) != 1 || s[0] != "Smith_wedding_1.DNG" {
		t.Fatalf("skipped %v", s)
	}
	if got := strings.Join(names(p, false), ","); got != "Smith_wedding_2.DNG" {
		t.Fatalf("names %s", got)
	}
}

func TestRenamePatternNeedsCounterOrOrig(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}})
	o := opts(t, src)
	o.Rename = "{date}_{name}"
	if _, err := MakePlan(o); err == nil {
		t.Fatal("pattern without {n} or {orig} accepted")
	}
	o.Rename = "{date}_{orig}"
	p, err := MakePlan(o)
	if err != nil || p.Files[0].Name != "20261002_M1.DNG" {
		t.Fatalf("{orig}: %v %+v", err, p)
	}
}

func TestPlanChecksFreeSpace(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {size: 5000}})
	o := opts(t, src)
	o.freeSpace = func(string) (uint64, error) { return 5000, nil }
	p, err := MakePlan(o)
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("free space not checked: %v", err)
	}
	if p == nil || p.Bytes != 5000 { // the plan still comes back, for --dry-run to show
		t.Fatalf("no plan with the space error: %+v", p)
	}
}

func TestPlanBackupDestination(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}})
	o := opts(t, src)
	o.Backup = t.TempDir()
	p, err := MakePlan(o)
	if err != nil || len(p.Dests) != 2 || filepath.Base(p.Dests[1]) != "2026-10-02 Smith wedding" {
		t.Fatalf("%v %+v", err, p)
	}
	card(t, p.Dests[1], map[string]spec{"M1.DNG": {seed: 3, mtime: t0.Add(time.Hour)}})
	if _, err := MakePlan(o); err == nil {
		t.Fatal("clash on the backup not refused")
	}
}

func TestPlanCopiesOnlyWhereMissing(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}})
	o := opts(t, src)
	o.Backup = t.TempDir()
	card(t, filepath.Join(o.Dest, "2026-10-02 Smith wedding"), map[string]spec{"M1.DNG": {}})
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	f := p.Files[0]
	if f.Skip != "" || len(f.To) != 1 || f.To[0] != p.Dests[1] {
		t.Fatalf("want a copy to the backup only, got skip=%q to=%v", f.Skip, f.To)
	}
}

// A destination that doesn't exist yet is measured on the nearest folder that does.
func TestPlanDestNotYetCreated(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {}})
	o := opts(t, src)
	o.freeSpace = nil // real statfs
	o.Dest = filepath.Join(t.TempDir(), "Pictures", "new")
	if _, err := MakePlan(o); err != nil {
		t.Fatal(err)
	}
}

// The M11-P numbers its M… and L… (Content Credentials) frames from one counter:
// --rename's {n} numbers them in that order (the counter), not by name, which puts
// every L before every M. A counter that wraps into the next DCF folder continues it.
func TestRenameNumbersInCameraOrder(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	var want []string
	for i, n := range []string{"M1102767", "M1102768", "M1102769", "M1102770", "M1102771",
		"L1002772", "L1002773", "L1002774", "L1002775", "L1002776"} {
		files["DCIM/100LEICA/"+n+".DNG"] = spec{seed: byte(i)}
		want = append(want, n)
	}
	files["DCIM/100LEICA/M1109999.DNG"] = spec{seed: 20}
	files["DCIM/101LEICA/M1100001.DNG"] = spec{seed: 21}
	want = append(want, "M1109999", "M1100001")
	card(t, src, files)
	o := opts(t, src)
	o.Rename = "{n:4}_{orig}"
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, f := range p.Files {
		got = append(got, f.Name)
		if exp := fmt.Sprintf("%04d_%s.DNG", i+1, want[i]); f.Name != exp {
			t.Errorf("file %d: %s, want %s", i, f.Name, exp)
		}
	}
	if t.Failed() {
		t.Fatalf("names %v", got)
	}
}

// --split-at names the first frame of each later event in camera order too.
func TestSplitAtCameraOrder(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/100LEICA/M1102770.DNG": {}, "DCIM/100LEICA/M1102771.DNG": {seed: 1},
		"DCIM/100LEICA/L1002772.DNG": {seed: 2}, "DCIM/100LEICA/L1002773.DNG": {seed: 3}})
	o := opts(t, src)
	o.SplitAt = []string{"L1002772"}
	ps, err := MakePlans(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || strings.Join(names(ps[0], false), ",") != "M1102770.DNG,M1102771.DNG" ||
		strings.Join(names(ps[1], false), ",") != "L1002772.DNG,L1002773.DNG" {
		for _, p := range ps {
			t.Logf("%s: %v", p.Folder, names(p, false))
		}
		t.Fatal("events not split in camera order")
	}
}

// Two cards (two bodies, both 100LEICA, overlapping counters) are numbered card by
// card in the order given, each in camera order: one numbering, never interleaved.
func TestRenameNumbersCardByCard(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	card(t, a, map[string]spec{"DCIM/100LEICA/L1000006.DNG": {seed: 1}, "DCIM/100LEICA/M1100005.DNG": {seed: 2}})
	card(t, b, map[string]spec{"DCIM/100LEICA/M1100007.DNG": {seed: 3}, "DCIM/100LEICA/M1100003.DNG": {seed: 4}})
	for _, tc := range []struct {
		sources []string
		want    string
	}{
		{[]string{a, b}, "1_M1100005.DNG,2_L1000006.DNG,3_M1100003.DNG,4_M1100007.DNG"},
		{[]string{b, a}, "1_M1100003.DNG,2_M1100007.DNG,3_M1100005.DNG,4_L1000006.DNG"},
	} {
		o := opts(t, tc.sources[0])
		o.Sources = tc.sources
		o.Rename = "{n}_{orig}"
		p, err := MakePlan(o)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(names(p, false), ","); got != tc.want {
			t.Errorf("sources %v: %s, want %s", tc.sources, got, tc.want)
		}
	}
}
