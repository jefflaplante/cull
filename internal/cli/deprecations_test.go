package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every retired form still runs and says what replaced it.
func TestDeprecations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CULL_CONFIG", "/dev/null")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	export := filepath.Join(t.TempDir(), "labels.jsonl")
	if err := os.WriteFile(export, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		hint string
	}{
		{[]string{"judge", "--estimate", "--resume", dir}, "--fresh"},
		{[]string{"judge", "--estimate", "--write-xmp", dir}, "--no-xmp"},
		{[]string{"judge", "--estimate", "--xmp-develop", dir}, "apply-c1"},
		{[]string{"judge", "--estimate", "--move-culled", dir}, "--sort=culls"},
		{[]string{"judge", "--estimate", "--project", "p", dir}, "cull tag"},
		{[]string{"scan", "--event", "e", dir}, "cull tag"},
		{[]string{"review", "--static", dir}, "cull review"},
		{[]string{"import-labels", export, dir}, "cull review"},
		{[]string{"rank", "--estimate", dir}, "cull judge"},
	}
	for _, c := range cases {
		out, _ := run(t, c.args...) // some refuse later for their own reasons; the warning comes first
		if !strings.Contains(out, "deprecated") || !strings.Contains(out, c.hint) {
			t.Errorf("%v: want a deprecation naming %q:\n%s", c.args, c.hint, out)
		}
	}
}
