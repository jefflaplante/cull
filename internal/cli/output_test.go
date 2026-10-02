package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

func TestDebugReachesTheBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)

	out, err := run(t, "judge", "--debug", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil {
		t.Fatalf("judge: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude-code: init apiKeySource=none") || !strings.Contains(out, "--system-prompt <") {
		t.Fatalf("--debug lacks backend detail:\n%s", out)
	}
	out, err = run(t, "judge", "--fresh", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil {
		t.Fatalf("judge: %v\n%s", err, out)
	}
	if strings.Contains(out, "claude-code: init") {
		t.Fatalf("backend detail without --debug:\n%s", out)
	}
}

func TestNoLiveViewWithoutTerminal(t *testing.T) {
	defer func(f func(io.Writer) bool) { outputIsTerminal = f }(outputIsTerminal)
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	cases := []struct {
		name     string
		terminal bool
		opts     outputOpts
		live     bool
		want     bool
	}{
		{"not a terminal", false, outputOpts{}, true, false},
		{"terminal", true, outputOpts{}, true, true},
		{"terminal, --plain", true, outputOpts{plain: true}, true, false},
		{"terminal, -q", true, outputOpts{quiet: true}, true, false},
		{"terminal, short command", true, outputOpts{}, false, false},
	}
	for _, c := range cases {
		outputIsTerminal = func(io.Writer) bool { return c.terminal }
		o := c.opts
		out := o.newOutput(cmd, c.live)
		if out.live != c.want {
			t.Errorf("%s: live=%v, want %v", c.name, out.live, c.want)
		}
		out.Close()
	}
}

// Moving DNGs is the one change cull makes to the shoot: -q must still say so.
func TestQuietStillReportsMoves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	out, err := run(t, "judge", "-q", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--move-culled", dir)
	if err != nil {
		t.Fatalf("judge: %v\n%s", err, out)
	}
	if !strings.Contains(out, "moved 1 culled frame(s)") || !strings.Contains(out, "cull restore") {
		t.Fatalf("-q hid the move:\n%s", out)
	}
}
