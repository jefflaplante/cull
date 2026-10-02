package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVerbosityFlagConflicts(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"quiet and verbose": {"scan", "-q", "-v", dir},
		"verbose and debug": {"scan", "-v", "--debug", dir},
		"log-level with -q": {"scan", "--log-level", "debug", "-q", dir},
		"unknown log level": {"scan", "--log-level", "loud", dir},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestQuietKeepsSummary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	out, err := run(t, "scan", "-q", dir)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if strings.Contains(out, "[1/1]") || strings.Contains(out, "DNGs found") {
		t.Fatalf("quiet printed progress:\n%s", out)
	}
	if !strings.Contains(out, "results:") || !strings.Contains(out, "report:") {
		t.Fatalf("quiet lost the summary:\n%s", out)
	}
}

func TestDefaultLevelKeepsProgressLines(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	out, err := run(t, "scan", dir)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[1/1]") || !strings.Contains(out, "1 DNGs found") {
		t.Fatalf("default level lost today's lines:\n%s", out)
	}
}

func TestVerboseShowsStages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	out, err := run(t, "scan", "-v", dir)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	if !strings.Contains(out, "scan: 1 frames") || !strings.Contains(out, "focus:") {
		t.Fatalf("verbose lacks the stage or per-frame detail:\n%s", out)
	}
	out, _ = run(t, "scan", dir)
	if strings.Contains(out, "scan: 1 frames") || strings.Contains(out, "focus:") {
		t.Fatalf("default level shows verbose lines:\n%s", out)
	}
}
