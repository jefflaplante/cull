package pipeline

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/report"
)

// sortedShoot judges the 3-frame shoot (L1 cull, L2 keep, L3 review) with sidecars
// and --sort.
func sortedShoot(t *testing.T) (string, Config) {
	t.Helper()
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled, c.Sort, c.WriteXMP = false, true, true
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

func TestSortPlacesByVerdict(t *testing.T) {
	dir, c := sortedShoot(t)
	for name, folder := range map[string]string{"L1000001": CullDir, "L1000002": KeepDir, "L1000003": ReviewDir} {
		for _, ext := range []string{".DNG", ".xmp"} {
			if !exists(filepath.Join(dir, folder, name+ext)) {
				t.Errorf("%s%s not in %s/", name, ext, folder)
			}
			if exists(filepath.Join(dir, name+ext)) {
				t.Errorf("%s%s left at home", name, ext)
			}
		}
	}
	rep, _ := report.Load(c.ReportPath)
	if r := result(t, rep, "L1000002.DNG"); r.MovedTo != filepath.Join(dir, KeepDir, "L1000002.DNG") || r.XMP != filepath.Join(dir, KeepDir, "L1000002.xmp") {
		t.Fatalf("not recorded: moved_to=%q xmp=%q", r.MovedTo, r.XMP)
	}
}

func TestSortLeavesErrorsHome(t *testing.T) {
	dir, b := shoot(t)
	os.WriteFile(filepath.Join(dir, "L1000009.DNG"), []byte("not a dng"), 0o644)
	c := moveCfg(dir)
	c.MoveCulled, c.Sort = false, true
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "L1000009.DNG")) {
		t.Fatal("a frame that failed was moved")
	}
}

func TestResortFollowsLabels(t *testing.T) {
	dir, c := sortedShoot(t)
	lab := map[string]labels.Entry{"L1000001.DNG": {File: "L1000001.DNG", Label: "keep"}}
	sum, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, Sort: true, Labels: lab}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, KeepDir, "L1000001.DNG")) || !exists(filepath.Join(dir, KeepDir, "L1000001.xmp")) {
		t.Fatal("relabelled frame (and sidecar) not moved cull/ → keep/")
	}
	if exists(filepath.Join(dir, CullDir)) {
		t.Fatal("empty cull/ left behind")
	}
	if sum.Moved != 1 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestSortNeverOverwrites(t *testing.T) {
	dir, b := shoot(t)
	os.MkdirAll(filepath.Join(dir, KeepDir), 0o755)
	stranger := filepath.Join(dir, KeepDir, "L1000002.DNG")
	os.WriteFile(stranger, []byte("someone else's"), 0o644)
	c := moveCfg(dir)
	c.MoveCulled, c.Sort = false, true
	var log bytes.Buffer
	c.Log = &log
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(stranger); string(got) != "someone else's" {
		t.Fatal("overwrote a file in keep/")
	}
	r := result(t, rep, "L1000002.DNG")
	if !exists(filepath.Join(dir, "L1000002.DNG")) || r.MovedTo != "" || !strings.Contains(strings.Join(r.Fixups, ";"), "move") {
		t.Fatalf("frame should stay home with a fixup: moved_to=%q fixups=%v", r.MovedTo, r.Fixups)
	}
	if !strings.Contains(log.String(), "not moved L1000002.DNG") {
		t.Fatalf("no warning:\n%s", log.String())
	}
}

func TestReconcileFindsSortedFrame(t *testing.T) {
	dir, c := sortedShoot(t)
	rep, _ := report.Load(c.ReportPath)
	r := result(t, rep, "L1000002.DNG")
	// moved by hand from keep/ to review/, report not told
	os.MkdirAll(filepath.Join(dir, ReviewDir), 0o755)
	os.Rename(filepath.Join(dir, KeepDir, "L1000002.DNG"), filepath.Join(dir, ReviewDir, "L1000002.DNG"))
	if !reconcileMove(&r) || r.MovedTo != filepath.Join(dir, ReviewDir, "L1000002.DNG") {
		t.Fatalf("not adopted: %q", r.MovedTo)
	}
	// two candidates: ambiguous, nothing changes
	r2 := result(t, rep, "L1000003.DNG")
	os.Rename(filepath.Join(dir, ReviewDir, "L1000003.DNG"), filepath.Join(dir, "L1000003.DNG"))
	data, _ := os.ReadFile(filepath.Join(dir, "L1000003.DNG"))
	os.WriteFile(filepath.Join(dir, KeepDir, "L1000003.DNG"), data, 0o644)
	st, _ := os.Stat(filepath.Join(dir, "L1000003.DNG"))
	os.Chtimes(filepath.Join(dir, KeepDir, "L1000003.DNG"), st.ModTime(), st.ModTime())
	if reconcileMove(&r2) {
		t.Fatalf("adopted one of two candidates: %q", r2.MovedTo)
	}
}

func TestRestoreFromSortFolders(t *testing.T) {
	dir, c := sortedShoot(t)
	// one frame moved by hand between sort folders: still restored
	os.Rename(filepath.Join(dir, KeepDir, "L1000002.DNG"), filepath.Join(dir, ReviewDir, "L1000002.DNG"))
	os.Rename(filepath.Join(dir, KeepDir, "L1000002.xmp"), filepath.Join(dir, ReviewDir, "L1000002.xmp"))
	n, err := Restore(c.ReportPath, dir, io.Discard, nil)
	if err != nil || n != 3 {
		t.Fatalf("restored %d, %v", n, err)
	}
	for _, name := range []string{"L1000001", "L1000002", "L1000003"} {
		if !exists(filepath.Join(dir, name+".DNG")) || !exists(filepath.Join(dir, name+".xmp")) {
			t.Errorf("%s not home", name)
		}
	}
	for _, d := range []string{KeepDir, ReviewDir, CullDir} {
		if exists(filepath.Join(dir, d)) {
			t.Errorf("%s/ left behind", d)
		}
	}
}

func TestDiscoverSkipsPlaceDirs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{CulledDir, KeepDir, ReviewDir, CullDir, "day2"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		os.WriteFile(filepath.Join(dir, d, "X.DNG"), nil, 0o644)
	}
	files, _ := Discover(dir, true)
	if len(files) != 1 || !strings.Contains(files[0], "day2") {
		t.Fatalf("discovered %v", files)
	}
}

func TestSortAndMoveCulledExclusive(t *testing.T) {
	_, c := culledShoot(t, nil)
	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, Sort: true, MoveCulled: true}, io.Discard); err == nil {
		t.Fatal("--sort with --move-culled accepted")
	}
}

func TestSortNeedsJudgedFrames(t *testing.T) {
	dir, _ := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled, c.DryRun = false, true
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, Sort: true}, io.Discard); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("sorting a scan-only report: %v", err)
	}
}

// offload --verify looks for copies in the same folders frames are moved into.
func TestOffloadKnowsThePlaceDirs(t *testing.T) {
	if strings.Join(offload.MovedDirs, ",") != strings.Join(placeDirs, ",") {
		t.Fatalf("offload.MovedDirs %v != placeDirs %v", offload.MovedDirs, placeDirs)
	}
}
