package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return buf.String(), err
}

func TestFlagValidation(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]string{
		"xmp-develop without write-xmp":   {"cull", "--xmp-develop", dir},
		"overwrite-xmp without write-xmp": {"cull", "--overwrite-xmp", dir},
		"bad min-crop-area":               {"cull", "--min-crop-area", "1.5", dir},
		"missing dir arg":                 {"cull"},
		"not a directory":                 {"scan", dir + "/nope"},
	}
	for name, args := range cases {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestScanEmptyDirNeedsNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	out, err := run(t, "scan", t.TempDir())
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0 DNGs found") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestCullWithoutKeyFails(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	if _, err := run(t, "cull", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("want no-API-key error, got %v", err)
	}
}
