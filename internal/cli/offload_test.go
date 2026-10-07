package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCard writes tiny DNGs under <tmp>/DCIM/100LEICA and returns the card root.
func fakeCard(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "DCIM", "100LEICA")
	os.MkdirAll(dir, 0o755)
	for _, n := range names {
		tinyDNG(t, filepath.Join(dir, n))
	}
	return root
}

func shootDirs(t *testing.T, dest string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dest)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestOffloadDryRunWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG", "M2.DNG"), t.TempDir()
	out, err := run(t, "offload", "--dry-run", "--name", "Test", "--date", "2026-10-02", cardDir, dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "2026-10-02 Test") || !strings.Contains(out, "2 of 2 DNGs to copy") {
		t.Fatalf("plan not printed:\n%s", out)
	}
	if d := shootDirs(t, dest); len(d) != 0 {
		t.Fatalf("dry run wrote %v", d)
	}
}

func TestOffloadCopiesThenScans(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest, backup := fakeCard(t, "M1.DNG"), t.TempDir(), t.TempDir()
	out, err := run(t, "offload", "--name", "Test", "--date", "2026-10-02", "--backup", backup, cardDir, dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	folder := filepath.Join(dest, "2026-10-02 Test")
	for _, want := range []string{"safe to format", "1 DNGs found", "cull judge --estimate"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, p := range []string{filepath.Join(folder, "M1.DNG"), filepath.Join(folder, "cull-report.json"), filepath.Join(backup, "2026-10-02 Test", "M1.DNG")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s:\n%s", p, out)
		}
	}
	if _, err := os.Stat(filepath.Join(backup, "2026-10-02 Test", "cull-report.json")); err == nil {
		t.Fatal("scanned the backup too")
	}
}

func TestOffloadRefusesClash(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG"), t.TempDir()
	folder := filepath.Join(dest, "2026-10-02")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "M1.DNG"), []byte("another frame"), 0o644)
	out, err := run(t, "offload", "--date", "2026-10-02", "--no-scan", cardDir, dest)
	if err == nil || !strings.Contains(err.Error()+out, "--rename") {
		t.Fatalf("clash not refused: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(folder, "M1.DNG")); string(b) != "another frame" {
		t.Fatal("existing file touched")
	}
}

func TestOffloadVerify(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG", "M2.DNG"), t.TempDir()
	if out, err := run(t, "offload", "--date", "2026-10-02", "--no-scan", cardDir, dest); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	folder := filepath.Join(dest, "2026-10-02")
	if out, err := run(t, "offload", "--verify", folder); err != nil || !strings.Contains(out, "2 verified") {
		t.Fatalf("verify of good copies: %v\n%s", err, out)
	}
	f := filepath.Join(folder, "M2.DNG")
	b, _ := os.ReadFile(f)
	b[len(b)-1] ^= 1
	os.WriteFile(f, b, 0o644)
	out, err := run(t, "offload", "--verify", folder)
	if err == nil || !strings.Contains(out, "M2.DNG") {
		t.Fatalf("corruption not reported: %v\n%s", err, out)
	}
}

func TestOffloadArgs(t *testing.T) {
	if _, err := run(t, "offload", t.TempDir()); err == nil {
		t.Fatal("one argument accepted without --verify")
	}
	if _, err := run(t, "offload", "--verify", t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("--verify with two arguments accepted")
	}
}

func TestSortFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if _, err := run(t, "judge", "--sort", "--move-culled", "--backend", "claude-code", "--claude-bin", bin, dir); err == nil {
		t.Fatal("judge --sort --move-culled accepted")
	}
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(t, "decide", "--sort", dir); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("decide --sort on a scan report: %v\n%s", err, out)
	}
	out, err := run(t, "judge", "--fresh", "--sort", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull", "L1000001.DNG")); err != nil || !strings.Contains(out, "sorted 1 frame(s)") {
		t.Fatalf("judge --sort didn't sort:\n%s", out)
	}
	if out, err := run(t, "restore", dir); err != nil || !strings.Contains(out, "restored 1") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
}

func TestDecideSortSaysSorted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := run(t, "decide", "--sort", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "sorted 1 into keep/, review/ and cull/") || strings.Contains(out, "culled/") {
		t.Fatalf("decide --sort summary:\n%s", out)
	}
}

// Tags given to offload are stored even when no scan follows the copy, so --no-scan
// doesn't silently drop them; a dry run says it stores nothing.
func TestOffloadNoScanStoresTags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG"), t.TempDir()
	out, err := run(t, "offload", "--no-scan", "--name", "Test", "--date", "2026-10-02", "--location", "Forest Park, Portland", "--keyword", "family", cardDir, dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	folder := filepath.Join(dest, "2026-10-02 Test")
	out, err = run(t, "tag", folder)
	if err != nil || !strings.Contains(out, "location: Forest Park, Portland") || !strings.Contains(out, "keywords: family") {
		t.Fatalf("tags not stored: %v\n%s", err, out)
	}
	// A later scan keeps them.
	if out, err := run(t, "scan", folder); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, _ := run(t, "tag", folder); !strings.Contains(out, "location: Forest Park, Portland") {
		t.Fatalf("scan dropped the tags:\n%s", out)
	}
}

func TestOffloadDryRunSaysTagsNotStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG"), t.TempDir()
	out, err := run(t, "offload", "--dry-run", "--name", "Test", "--event", "Ceremony", cardDir, dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "not stored") {
		t.Fatalf("no note that the dry run stores no tags:\n%s", out)
	}
}

// A dry run's plan is its whole output, so -q still prints it.
func TestOffloadQuietDryRunPrintsPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG"), t.TempDir()
	out, err := run(t, "offload", "-q", "--dry-run", "--name", "Test", cardDir, dest)
	if err != nil || !strings.Contains(out, "1 of 1 DNGs to copy") {
		t.Fatalf("%v\n%q", err, out)
	}
}

// --split: two events five hours apart become two numbered shoot folders, each copied,
// verified and scanned on its own.
func TestOffloadSplitIntoEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG", "M2.DNG", "M3.DNG"), t.TempDir()
	day := time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)
	for i, n := range []string{"M1.DNG", "M2.DNG", "M3.DNG"} {
		at := day.Add(time.Duration(i) * time.Minute)
		if i == 2 {
			at = day.Add(5 * time.Hour)
		}
		os.Chtimes(filepath.Join(cardDir, "DCIM", "100LEICA", n), at, at)
	}
	out, err := run(t, "offload", "--dry-run", "--split", "--name", "Test", cardDir, dest)
	if err != nil || !strings.Contains(out, "2 events, split where capture time jumps by more than 2h") || !strings.Contains(out, "event 2: shoot folder: 2026-10-02 Test 2") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	out, err = run(t, "offload", "--split", "--name", "Test", cardDir, dest)
	if err != nil || !strings.Contains(out, "all 3 files verified in 2 shoot folders: safe to format the card") {
		t.Fatalf("%v\n%s", err, out)
	}
	for folder, n := range map[string]string{"2026-10-02 Test 1": "M1.DNG", "2026-10-02 Test 2": "M3.DNG"} {
		for _, f := range []string{n, "cull-report.json", "cull-offload.jsonl"} {
			if _, err := os.Stat(filepath.Join(dest, folder, f)); err != nil {
				t.Errorf("%s/%s: %v", folder, f, err)
			}
		}
	}
}

func TestOffloadSetDate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir, dest := fakeCard(t, "M1.DNG"), t.TempDir()
	out, err := run(t, "offload", "--set-date", "2026-10-04", "--time", "09:30:00", "--name", "Test", "--no-scan", cardDir, dest)
	if err != nil || !strings.Contains(out, "safe to format") {
		t.Fatalf("%v\n%s", err, out)
	}
	st, err := os.Stat(filepath.Join(dest, "2026-10-04 Test", "M1.DNG"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := time.Date(2026, 10, 4, 9, 30, 0, 0, time.Local); !st.ModTime().Equal(want) {
		t.Fatalf("mtime %v, want %v", st.ModTime(), want)
	}
}

func TestOffloadSetDateFlagErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cardDir := fakeCard(t, "M1.DNG")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--set-date", "2026-10-04", "--date", "2026-10-03"}, "--set-date"},
		{[]string{"--time", "09:30:00"}, "--time"},
		{[]string{"--set-date", "2026-13-04"}, "--set-date"},
		{[]string{"--set-date", "2026-10-04", "--time", "9:30"}, "--time"},
	} {
		dest := t.TempDir()
		out, err := run(t, append(append([]string{"offload", "--no-scan"}, c.args...), cardDir, dest)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err %v\n%s", c.args, err, out)
		}
		if d := shootDirs(t, dest); len(d) != 0 {
			t.Errorf("%v: wrote %v", c.args, d)
		}
	}
	// The same date twice is fine.
	if out, err := run(t, "offload", "--dry-run", "--set-date", "2026-10-04", "--date", "2026-10-04", cardDir, t.TempDir()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestSetDateNotInDotfile(t *testing.T) {
	for _, k := range []string{"set-date", "time"} {
		if !notInDotfile[k] {
			t.Errorf("%s settable from the dotfile", k)
		}
	}
}
