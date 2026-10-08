package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/develop"
	"github.com/jefflaplante/cull/internal/develop/lctest"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/xmp"
)

// developShoot is a shoot with no report: two M10-R frames in raw/, A labelled keep
// (with a sidecar EV), B labelled cull. The fake LightCraft (lctest) stands in.
func developShoot(t *testing.T) (dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = t.TempDir()
	for i, n := range []string{"A.DNG", "B.DNG"} {
		p := filepath.Join(dir, "raw", n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, dngtest.Build(t, dngtest.Fixture{Model: "LEICA M10-R", Payload: []byte{byte(i)}}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ev := 0.5
	if err := xmp.Write(xmp.Path(filepath.Join(dir, "raw", "A.DNG")), xmp.Sidecar{Label: "Green", ExposureEV: &ev}, false); err != nil {
		t.Fatal(err)
	}
	lp := filepath.Join(dir, labels.FileName)
	labels.Append(lp, labels.Entry{File: "A.DNG", Label: "keep"})
	labels.Append(lp, labels.Entry{File: "B.DNG", Label: "cull"})
	t.Setenv(lctest.Env, "1")
	t.Setenv("FAKE_LC_LOG", filepath.Join(t.TempDir(), "lc.log"))
	return dir
}

func TestDevelopDryRunWritesNothing(t *testing.T) {
	dir := developShoot(t)
	out, err := run(t, "develop", "-r", "--dry-run", "--lightcraft", os.Args[0], dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"1 keep(s): 0 up to date, 1 to develop", "LEICA M10-R → leica-m10r-std: 1 frame(s)", "estimate: ~25s", "peak memory ~2.9 GB", "dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, p := range []string{develop.StateName, develop.WorkName, "export"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dry run wrote %s", p)
		}
	}
}

func TestDevelopNeedsYesWithoutTerminal(t *testing.T) {
	dir := developShoot(t)
	defer func(f func(*os.File) bool) { isTerminal = f }(isTerminal)
	isTerminal = func(*os.File) bool { return false }
	out, err := run(t, "develop", "-r", "--lightcraft", os.Args[0], dir)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "export")); !errors.Is(err, os.ErrNotExist) {
		t.Error("developed without --yes")
	}
}

func TestDevelopAsksOnTerminal(t *testing.T) {
	dir := developShoot(t)
	defer func(f func(*os.File) bool) { isTerminal = f }(isTerminal)
	isTerminal = func(*os.File) bool { return true }
	cmd := NewRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"develop", "-r", "--lightcraft", os.Args[0], dir})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("err %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Develop 1 frame(s), ~25s? [y/N]") {
		t.Errorf("no question in\n%s", buf.String())
	}
}

func TestDevelopEndToEnd(t *testing.T) {
	dir := developShoot(t)
	out, err := run(t, "develop", "-r", "--yes", "--lightcraft", os.Args[0], dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "developed 1, failed 0, up to date 0: 1 JPEG(s)") {
		t.Errorf("summary missing in\n%s", out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "export", "A.jpg"))
	if err != nil || !strings.Contains(string(b), "A.DNG") {
		t.Fatalf("A.jpg: %v %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "export", "B.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the cull was developed")
	}
	log, _ := os.ReadFile(os.Getenv("FAKE_LC_LOG"))
	if !strings.Contains(string(log), `"value":0.8`) {
		t.Errorf("A's EV (+0.5 on the preset's +0.3) not set: %s", log)
	}
	// Up to date: a second run starts nothing.
	out, err = run(t, "develop", "-r", "--yes", "--lightcraft", os.Args[0], dir)
	if err != nil || !strings.Contains(out, "1 keep(s): 1 up to date, 0 to develop") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
}

func TestDevelopRefusals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	empty := t.TempDir()
	if _, err := run(t, "develop", "--dry-run", empty); err == nil || !strings.Contains(err.Error(), "no report") {
		t.Errorf("no report, no labels: %v", err)
	}
	dir := developShoot(t)
	for name, args := range map[string][]string{
		"bad wb":      {"--wb", "warm"},
		"bad quality": {"--quality", "0"},
		"bad jobs":    {"-j", "0"},
	} {
		if _, err := run(t, append(append([]string{"develop", "-r", "--dry-run", "--lightcraft", os.Args[0]}, args...), dir)...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := run(t, "develop", "-r", "--yes", dir); err == nil || !strings.Contains(err.Error(), "lightcraft-cli isn't on your PATH") {
		t.Errorf("no lightcraft-cli: %v", err)
	}
}
