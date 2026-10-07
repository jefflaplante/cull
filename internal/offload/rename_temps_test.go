package offload

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rename temp is ".cull-rename-<8 hex>.<name>": never "._…" (an AppleDouble
// companion), whatever the frame's name, and RenameTemps maps each back to that name,
// skipping companions ("._.cull-rename-…"), redate's and offload's temps.
func TestRenameTempNames(t *testing.T) {
	for _, n := range []string{"_IGP0001.DNG", "M1.DNG", "_DSC0001.xmp", "_.DNG"} {
		tmp := RenameTempName(n)
		if strings.HasPrefix(tmp, "._") || !strings.HasPrefix(tmp, RenameTempPrefix) || !strings.HasSuffix(tmp, "."+n) {
			t.Errorf("%s → %s", n, tmp)
		}
		if RenameTempName(n) == tmp {
			t.Errorf("%s: two temps share a name", n)
		}
	}
	dir := t.TempDir()
	for _, n := range []string{
		".cull-rename-01020304._IGP0001.DNG", ".cull-rename-0a0b0c0d.M1.xmp",
		"._.cull-rename-01020304._IGP0001.DNG", ".cull-redate-0a0b0c0d.M1.DNG",
		"._IGP0001.DNG.cull-01020304.tmp", ".cull-rename-xyz.M1.DNG", ".cull-rename-01020304.", ".cull-rename-01020304..x",
	} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	got := RenameTemps(dir)
	want := map[string]string{
		filepath.Join(dir, ".cull-rename-01020304._IGP0001.DNG"): "_IGP0001.DNG",
		filepath.Join(dir, ".cull-rename-0a0b0c0d.M1.xmp"):       "M1.xmp",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s → %q, want %q", k, got[k], v)
		}
	}
	// offload's sweep never takes one, nor redate's.
	RemoveStaleTemps(dir)
	for k := range want {
		if _, err := os.Stat(k); err != nil {
			t.Errorf("swept %s", k)
		}
	}
}

// MoveNoReplace gives src the name dst only when dst is free, and leaves one name.
func TestMoveNoReplace(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(c, []byte("C"), 0o644)
	if err := MoveNoReplace(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("old name kept")
	}
	if got, _ := os.ReadFile(b); string(got) != "A" {
		t.Fatalf("b = %q", got)
	}
	if err := MoveNoReplace(b, c); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("replaced: %v", err)
	}
	if got, _ := os.ReadFile(c); string(got) != "C" {
		t.Fatal("c changed")
	}
	if got, _ := os.ReadFile(b); string(got) != "A" {
		t.Fatal("b gone")
	}
}

// The pattern tokenizer offload --rename and cull rename share.
func TestExpandName(t *testing.T) {
	got, err := ExpandName("{date}_{name}_{n:4}_{orig}", "20261004", "Smith wedding", "M1103817", 7)
	if err != nil || got != "20261004_Smith_wedding_0007_M1103817" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"{name}", "a/{n}", "{orig:3}", "{x}{n}", ".{n}", "{date}:{n}"} {
		if err := ValidatePattern(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, err := ExpandName(bad, "d", "n", "o", 1); err == nil {
			t.Errorf("ExpandName %q accepted", bad)
		}
	}
}
