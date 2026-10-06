package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSortModeValues(t *testing.T) {
	for in, want := range map[string]sortMode{"all": sortAll, "true": sortAll, "culls": sortCulls, "false": sortNone, "": sortNone, "True": sortAll, "TRUE": sortAll, "t": sortAll, "T": sortAll, "1": sortAll, "False": sortNone, "FALSE": sortNone, "f": sortNone, "F": sortNone, "0": sortNone} {
		var m sortMode
		if err := m.Set(in); err != nil || m != want {
			t.Errorf("Set(%q) = %q, %v; want %q", in, m, err, want)
		}
	}
	var m sortMode
	if err := m.Set("keep"); err == nil {
		t.Error("an unknown value must fail")
	}
}

// `--sort culls <dir>` (a space) would take "culls" as the folder: say what to type.
func TestSortWithSpaceNamesTheEqualsForm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{{"decide", "--sort", "culls", dir}, {"judge", "--estimate", "--sort", "culls", dir},
		{"decide", dir, "--sort", "culls"}, {"judge", dir, "--estimate", "--sort", "all"}, {"decide", "--sort", "culls"}} {
		_, err := run(t, args...)
		want := "--sort=culls"
		if strings.Contains(strings.Join(args, " "), "sort all") {
			want = "--sort=all"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

// The deprecated --move-culled still works, as --sort=culls, into cull/.
func TestMoveCulledIsSortCulls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--move-culled", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "--move-culled has been deprecated") || !strings.Contains(out, "--sort=culls") {
		t.Errorf("no deprecation warning naming --sort=culls:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull", "L1000001.DNG")); err != nil {
		t.Fatalf("not in cull/:\n%s", out)
	}
}
