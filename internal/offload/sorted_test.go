package offload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sortInto moves copies inside a shoot folder the way --sort and --move-culled do.
func sortInto(t *testing.T, folder string, moves map[string]string) {
	t.Helper()
	for name, sub := range moves {
		if err := os.MkdirAll(filepath.Join(folder, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(folder, name), filepath.Join(folder, sub, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// A second card offloaded after --sort continues the numbering past the sorted
// frames, instead of reusing their numbers (which made --verify report a mismatch).
func TestRenameCounterSeesSortedFrames(t *testing.T) {
	card1, card2 := t.TempDir(), t.TempDir()
	card(t, card1, map[string]spec{"DCIM/M1.DNG": {seed: 1}, "DCIM/M2.DNG": {seed: 2}})
	card(t, card2, map[string]spec{"DCIM/M9.DNG": {seed: 9}})
	o := opts(t, card1)
	o.Rename = "{date}_{name}_{n:4}"
	o.Date = "2026-10-02"
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("first card: %+v %v", res, err)
	}
	folder := p.Dests[0]
	sortInto(t, folder, map[string]string{"20261002_Smith_wedding_0001.DNG": "keep", "20261002_Smith_wedding_0002.DNG": "cull"})

	o.Sources = []string{card2}
	p, err = MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(p, false), ","); got != "20261002_Smith_wedding_0003.DNG" {
		t.Fatalf("second card named %s", got)
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("second card: %+v %v", res, err)
	}
	v, err := Verify(context.Background(), folder, nil)
	if err != nil || v.OK != 3 || v.Bad != 0 || len(v.Unrecorded) != 0 {
		t.Fatalf("verify %+v %v", v, err)
	}
}

// Re-offloading a card whose frames were sorted copies nothing: the copies in
// keep/ review/ cull/ culled/ count, verified by the manifest.
func TestCameraNamesSeesSortedFrames(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {seed: 1}, "DCIM/M2.DNG": {seed: 2}, "DCIM/M3.DNG": {seed: 3}})
	o := opts(t, src)
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("first run: %+v %v", res, err)
	}
	sortInto(t, p.Dests[0], map[string]string{"M1.DNG": "keep", "M2.DNG": "culled"})

	p, err = MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p, false); len(got) != 0 {
		t.Fatalf("would copy again: %v", got)
	}
	for _, f := range p.Files {
		if f.Unverified {
			t.Fatalf("%s unverified, but the manifest records it", f.Name)
		}
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe || res.Copied != 0 {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(p.Dests[0], "M1.DNG")); err == nil {
		t.Fatal("M1.DNG copied back into the shoot folder beside keep/M1.DNG")
	}
}

// A sorted copy that differs from the card is still a clash, not a skip.
func TestCameraNamesSortedClash(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {seed: 1}})
	o := opts(t, src)
	folder := filepath.Join(o.Dest, "2026-10-02 Smith wedding")
	card(t, folder, map[string]spec{"review/M1.DNG": {seed: 7, size: 2000}})
	if _, err := MakePlan(o); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("want a clash, got %v", err)
	}
}

// With --rename, a card copied before and then sorted is skipped by its manifest
// line, not copied again under a new number.
func TestRenameManifestSkipSeesSortedFrames(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M1.DNG": {seed: 1}, "DCIM/M2.DNG": {seed: 2}})
	o := opts(t, src)
	o.Rename = "{name}_{n}"
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Run(context.Background(), p, nil); err != nil || !res.Safe {
		t.Fatalf("first run: %+v %v", res, err)
	}
	sortInto(t, p.Dests[0], map[string]string{"Smith_wedding_1.DNG": "keep", "Smith_wedding_2.DNG": "review"})
	p, err = MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p, false); len(got) != 0 {
		t.Fatalf("would copy again: %v", got)
	}
	for _, f := range p.Files {
		if f.Unverified {
			t.Fatalf("%s unverified", f.Name)
		}
	}
}
