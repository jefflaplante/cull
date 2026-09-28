package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
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

func TestRestoreWithoutReportFails(t *testing.T) {
	_, err := run(t, "restore", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "report") {
		t.Fatalf("want a missing-report error, got %v", err)
	}
}

func TestCullHasMoveCulledFlag(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "cull" {
			if f := c.Flags().Lookup("move-culled"); f == nil || f.DefValue != "false" {
				t.Fatalf("move-culled flag: %+v", f)
			}
			return
		}
	}
	t.Fatal("no cull command")
}

// tinyDNG writes a minimal DNG: IFD0 marked reduced-resolution, strip = a JPEG.
func tinyDNG(t *testing.T, path string) {
	t.Helper()
	var j bytes.Buffer
	jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 1600, 1067)), nil)
	le := binary.LittleEndian
	var b bytes.Buffer
	b.WriteString("II")
	binary.Write(&b, le, uint16(42))
	binary.Write(&b, le, uint32(8))
	dataOff := uint32(8 + 2 + 4*12 + 4)
	binary.Write(&b, le, uint16(4))
	for _, e := range [][3]uint32{{0x00FE, 4, 1}, {0x0103, 3, 7}, {0x0111, 4, dataOff}, {0x0117, 4, uint32(j.Len())}} {
		binary.Write(&b, le, uint16(e[0]))
		binary.Write(&b, le, uint16(e[1]))
		binary.Write(&b, le, uint32(1))
		binary.Write(&b, le, e[2])
	}
	binary.Write(&b, le, uint32(0))
	b.Write(j.Bytes())
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fake `claude` that always judges the frame missed_focus.
const fakeClaudeCull = `#!/bin/sh
cat > /dev/null
echo '{"type":"system","subtype":"init","apiKeySource":"none"}'
echo '{"type":"result","is_error":false,"structured_output":{"sharpness":{"score":2,"status":"missed_focus","focus_target":"x"},"exposure":{"score":7,"status":"good","ev_adjust":0,"clipping":"none","reason":""},"composition":{"score":6,"status":"good","issues":[],"crop":{"apply":false,"left":0,"top":0,"right":1,"bottom":1},"straighten_degrees":0},"people":{"present":true,"eyes":"open","expression":"good"},"notes":""},"usage":{"input_tokens":10,"output_tokens":5}}'
`

func TestCullMoveCulledThenRestoreEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	frame := filepath.Join(dir, "L1000001.DNG")
	tinyDNG(t, frame)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)

	out, err := run(t, "cull", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--write-xmp", "--move-culled", dir)
	if err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	moved := filepath.Join(dir, "culled", "L1000001.DNG")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("frame not moved into culled/:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "culled", "L1000001.xmp")); err != nil {
		t.Fatal("sidecar not moved with the frame")
	}
	if !strings.Contains(out, "gophotocull restore") {
		t.Fatalf("no undo hint in output:\n%s", out)
	}

	if out, err := run(t, "restore", dir); err != nil || !strings.Contains(out, "restored 1") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if _, err := os.Stat(frame); err != nil {
		t.Fatal("frame not restored")
	}
}

func TestPolicyFlagValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"bad eyes action":       {"cull", "--eyes-closed", "delete", dir},
		"bad duplicates":        {"decide", "--duplicates", "burn", dir},
		"bad raw action":        {"cull", "--raw-clipped", "maybe", dir},
		"batch needs anthropic": {"cull", "--batch", "--backend", "openai", "--model", "m", dir},
		"batch with escalation": {"cull", "--batch", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", dir},
		"bad raw threshold":     {"cull", "--raw-clip-threshold", "150", dir},
		"negative burst gap":    {"scan", "--burst-gap", "-1s", dir},
		"bad escalate backend":  {"cull", "--escalate-backend", "gpt", "--escalate-model", "x", dir},
		"escalate needs model":  {"cull", "--escalate-backend", "anthropic", dir},
		"bad escalate-on":       {"cull", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", "--escalate-on", "blurry", dir},
		"negative sharp floor":  {"cull", "--review-below-sharpness", "-1", dir},
	} {
		if _, err := run(t, args...); err == nil || strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
}

func TestCullEstimateNeedsNoKeyAndCallsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	tinyDNG(t, filepath.Join(dir, "L2.DNG"))
	out, err := run(t, "cull", "--estimate", dir)
	if err != nil || !strings.Contains(out, "estimate: 2 frames") || !strings.Contains(out, "$") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "gophotocull-report.json")); err == nil {
		t.Fatal("--estimate must not run the pipeline")
	}
	out, err = run(t, "cull", "--estimate", "--backend", "openai", "--model", "m", dir)
	if err != nil || !strings.Contains(out, "no per-token cost") {
		t.Fatalf("openai estimate: err=%v\n%s", err, out)
	}
}

func TestDecideCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := run(t, "decide", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing report: %v", err)
	}
	dir := t.TempDir()
	frame := filepath.Join(dir, "L1000001.DNG")
	tinyDNG(t, frame)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "cull", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	out, err := run(t, "decide", "--eyes-closed", "cull", dir)
	if err != nil || !strings.Contains(out, "decided 1 frame(s); no decision changed") {
		t.Fatalf("decide: %v\n%s", err, out)
	}
}

func TestReviewCommandBuildsSheetNextToReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := run(t, "review", t.TempDir()); err == nil {
		t.Fatal("review without a report should fail")
	}
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	out, err := run(t, "review", dir)
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "gophotocull-review", "index.html")); err != nil || !strings.Contains(out, "index.html") {
		t.Fatalf("no sheet:\n%s", out)
	}
}

func TestCalibrateCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "cull", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	labels := filepath.Join(t.TempDir(), "labels.csv")
	os.WriteFile(labels, []byte("file,label\nL1000001.DNG,keep\n"), 0o644)
	out, err := run(t, "calibrate", "--labels", labels, filepath.Join(dir, "gophotocull-report.json"))
	if err != nil || !strings.Contains(out, "false-cull rate (keep → cull):   1/1") || !strings.Contains(out, "sweep") {
		t.Fatalf("calibrate: %v\n%s", err, out)
	}
	if _, err := run(t, "calibrate", filepath.Join(dir, "gophotocull-report.json")); err == nil {
		t.Fatal("calibrate without --labels should fail")
	}
}

func TestRawClipFlagDefaults(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		want := map[string]string{"cull": "true", "scan": "false"}[c.Name()]
		if want == "" {
			continue
		}
		if f := c.Flags().Lookup("raw-clip"); f == nil || f.DefValue != want {
			t.Errorf("%s --raw-clip default: %+v, want %s", c.Name(), f, want)
		}
	}
}

func TestApplyC1DryRunProbeAndRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "cull", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	out, err := run(t, "apply-c1", dir)
	if err != nil || !strings.Contains(out, `tell application "Capture One"`) || !strings.Contains(out, `"L1000001.DNG"`) || !strings.Contains(out, "set rating of v to 1") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if out, err := run(t, "apply-c1", "--probe", dir); err != nil || !strings.Contains(out, "image name: ") {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	osa := filepath.Join(t.TempDir(), "osascript")
	os.WriteFile(osa, []byte("#!/bin/sh\ncat >/dev/null\necho applied\n"), 0o755)
	if out, err := run(t, "apply-c1", "--run", "--osascript", osa, dir); err != nil || !strings.Contains(out, "applied") {
		t.Fatalf("run: %v\n%s", err, out)
	}
}
