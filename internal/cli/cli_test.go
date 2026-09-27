package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/report"
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

func TestBackendFlagValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cases := []struct {
		name   string
		args   []string
		errHas string
	}{
		{"unknown backend", []string{"cull", "--backend", "bogus", dir}, "unknown --backend"},
		{"openai needs model", []string{"cull", "--backend", "openai", dir}, "requires --model"},
		{"quota-stop range", []string{"cull", "--quota-stop", "0", dir}, "--quota-stop must be in (0, 1]"},
		{"locate value", []string{"cull", "--locate", "maybe", dir}, "--locate must be model or off"},
		{"missing claude binary", []string{"cull", "--backend", "claude-code", "--claude-bin", "/nonexistent/claude", dir}, "not found"},
	}
	for _, c := range cases {
		_, err := run(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.errHas) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.errHas, err)
		}
	}
}

func TestOpenAIBackendNeedsNoKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	out, err := run(t, "cull", "--backend", "openai", "--model", "m", t.TempDir())
	if err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	if !strings.Contains(out, "backend: openai") || !strings.Contains(out, "0 DNGs found") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestSaveInputsRefusesShootDir(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, "scan", "--save-inputs", dir, dir)
	if err == nil || !strings.Contains(err.Error(), "refusing to write inputs into the shoot directory") {
		t.Fatalf("got %v", err)
	}
}

func TestFaceMinQDefault(t *testing.T) {
	root := NewRootCmd()
	f := root.PersistentFlags().Lookup("face-min-q")
	if f == nil || f.DefValue != "80" {
		t.Fatalf("face-min-q flag: %+v", f)
	}
}

func TestScanSummary(t *testing.T) {
	rep := &report.Report{Results: []report.Result{
		{Preview: &report.PreviewInfo{Width: 9504, Height: 6320, Orientation: 8, Source: "tiff-ifd"}, FocusTarget: &report.FocusTarget{Source: "face"}},
		{Preview: &report.PreviewInfo{Width: 9504, Height: 6320, Orientation: 8, Source: "tiff-ifd"}, FocusTarget: &report.FocusTarget{Source: "none"}},
		{Preview: &report.PreviewInfo{Width: 1333, Height: 2000, Orientation: 1, Source: "exiftool:PreviewImage"}, FocusTarget: &report.FocusTarget{Source: "face"}},
		{Error: "preview: no JPEG"},
	}}
	want := "previews: long edge min/median/max = 2000/9504/9504 px; sources: exiftool:PreviewImage=1 tiff-ifd=2; orientation: 1=1 8=2; faces: 2/3"
	if got := ScanSummary(rep); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestScanConcurrencyDefaultBoundsMemory(t *testing.T) {
	// ~1 GB per in-flight 60MP frame (measured): keep scan's default at cull's.
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "scan" {
			if f := c.Flags().Lookup("concurrency"); f == nil || f.DefValue != "4" {
				t.Fatalf("scan --concurrency default: %+v", f)
			}
			return
		}
	}
	t.Fatal("no scan command")
}
