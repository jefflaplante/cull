package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
