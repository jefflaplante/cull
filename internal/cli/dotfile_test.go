package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
)

// TestMain keeps every test off the developer's own ~/.cull.
func TestMain(m *testing.M) {
	os.Setenv("CULL_CONFIG", filepath.Join(os.TempDir(), "cull-tests-no-dotfile"))
	os.Exit(m.Run())
}

func dotfile(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cull")
	os.WriteFile(p, []byte(body), 0o644)
	t.Setenv("CULL_CONFIG", p)
}

func decisionOf(t *testing.T, dir, name string) eval.Decision {
	t.Helper()
	rep, err := report.Load(filepath.Join(dir, "cull-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if filepath.Base(r.File) == name {
			return r.Decision
		}
	}
	t.Fatalf("no %s", name)
	return ""
}

// The dotfile sets a flag's default, and says so; a flag typed on the command line
// still wins. (L1 is sharp at 9: a review-below-sharpness of 9.5 sends it to review.)
func TestDotfileSetsDefaults(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "# mine\nreview-below-sharpness = 9.5\n")
	out, err := run(t, "decide", dir)
	if err != nil || !strings.Contains(out, "--review-below-sharpness 9.5") {
		t.Fatalf("%v\n%s", err, out)
	}
	if d := decisionOf(t, dir, "L1.DNG"); d != eval.Review {
		t.Fatalf("dotfile not applied: L1 %s", d)
	}
	if _, err := run(t, "decide", "--review-below-sharpness", "0", dir); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, dir, "L1.DNG"); d != eval.Keep {
		t.Fatalf("a typed flag didn't win: L1 %s", d)
	}
}

// A shoot's stored policy outranks the dotfile: tuning done on a shoot carries over,
// as it always has; the dotfile only replaces the built-in defaults.
func TestStoredPolicyBeatsDotfile(t *testing.T) {
	dir := judgedShoot(t)
	if _, err := run(t, "decide", "--review-below-sharpness", "9.5", dir); err != nil { // stored: 9.5
		t.Fatal(err)
	}
	dotfile(t, "review-below-sharpness = 0\n")
	if _, err := run(t, "decide", dir); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, dir, "L1.DNG"); d != eval.Review {
		t.Fatalf("the dotfile overrode the stored policy: L1 %s", d)
	}
}

// A [command] section applies to that command only.
func TestDotfileSections(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "[judge]\nreview-below-sharpness = 9.5\n")
	if _, err := run(t, "decide", dir); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, dir, "L1.DNG"); d != eval.Keep {
		t.Fatalf("a [judge] setting reached decide: L1 %s", d)
	}
}

// One-off and risky flags are refused with a warning; an unknown name warns (a typo
// mustn't pass silently); a bad value is an error naming the line.
func TestDotfileWarningsAndErrors(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "yes = true\nkep-best = 3\n")
	out, err := run(t, "decide", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"yes: not allowed", "kep-best: no command has this flag"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	dotfile(t, "\nkeep-best = lots\n")
	if _, err := run(t, "decide", dir); err == nil || !strings.Contains(err.Error(), ":2") || !strings.Contains(err.Error(), "keep-best") {
		t.Fatalf("bad value: %v", err)
	}
}

// Deprecated keys warn and still apply: write-xmp = false means no sidecars,
// sort = true means --sort=all, and resume = true changes nothing.
func TestDotfileDeprecatedKeys(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "write-xmp = false\nresume = true\nsort = true\n")
	out, err := run(t, "decide", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "write-xmp: deprecated") {
		t.Errorf("no deprecation warning for write-xmp:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.DNG")); err != nil {
		t.Fatalf("sort = true did not sort into keep/ review/ cull/:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.xmp")); err == nil {
		t.Fatal("write-xmp = false still wrote a sidecar")
	}
	out, err = run(t, "judge", "--estimate", dir)
	if err != nil || !strings.Contains(out, "resume: deprecated") {
		t.Fatalf("resume key: %v\n%s", err, out)
	}
}

func TestDotfileSortBoolWarns(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "sort = true\n")
	out, err := run(t, "decide", dir)
	if err != nil || !strings.Contains(out, "sort: deprecated") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.DNG")); err != nil {
		t.Fatal("sort = true did not sort all")
	}
}

func TestDotfileMoveCulled(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "move-culled = true\n")
	out, err := run(t, "decide", dir)
	if err != nil || !strings.Contains(out, "move-culled: deprecated") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull", "L3.DNG")); err != nil {
		t.Fatalf("L3 not in cull/:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1.DNG")); err != nil {
		t.Fatal("L1 left home expected")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err == nil {
		t.Fatal("keep/ should not exist")
	}
}

func TestDotfileSortAndMoveCulled(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "sort = true\nmove-culled = true\n")
	if out, err := run(t, "decide", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.DNG")); err != nil {
		t.Fatal("sort should win: L1 in keep/")
	}
	dir2 := judgedShoot(t)
	if out, err := run(t, "decide", "--sort", "--move-culled", dir2); err == nil {
		t.Fatalf("typed both should fail:\n%s", out)
	}
}

// A dotfile backend = openai (no model) mustn't fail continuing a claude-code report:
// the report's backend replaces it before validation.
func TestDotfileBackendWithoutModelContinuesReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin, _ := countingClaude(t)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	dotfile(t, "backend = openai\n")
	out, err := run(t, "judge", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil || !strings.Contains(out, "backend: claude-code") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	// Not continuing, the dotfile's openai still needs a model.
	if _, err := run(t, "judge", "--estimate", t.TempDir()); err == nil || !strings.Contains(err.Error(), "requires --model") {
		t.Fatalf("got %v", err)
	}
}

// A typed --rerank wins over a dotfile no-rank = true; only both typed contradict.
func TestTypedRerankBeatsDotfileNoRank(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dotfile(t, "no-rank = true\n")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	out, err := run(t, "judge", "--estimate", "--rerank", "--backend", "claude-code", dir)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if _, err := run(t, "judge", "--estimate", "--rerank", "--no-rank", "--backend", "claude-code", dir); err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("both typed: %v", err)
	}
}
