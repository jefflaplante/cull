package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
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
		"xmp-develop without write-xmp":   {"judge", "--xmp-develop", dir},
		"overwrite-xmp without write-xmp": {"judge", "--overwrite-xmp", dir},
		"bad min-crop-area":               {"judge", "--min-crop-area", "1.5", dir},
		"missing dir arg":                 {"judge"},
		"not a directory":                 {"scan", dir + "/nope"},
		"fresh with resume":               {"judge", "--fresh", "--resume", dir},
	}
	for name, args := range cases {
		if _, err := run(t, args...); err == nil || strings.Contains(err.Error(), "unknown command") {
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
	if _, err := run(t, "judge", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no API key") {
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
		{"unknown backend", []string{"judge", "--backend", "bogus", dir}, "unknown --backend"},
		{"openai needs model", []string{"judge", "--backend", "openai", dir}, "requires --model"},
		{"quota-stop range", []string{"judge", "--quota-stop", "0", dir}, "--quota-stop must be in (0, 1]"},
		{"locate value", []string{"judge", "--locate", "maybe", dir}, "--locate must be model or off"},
		{"missing claude binary", []string{"judge", "--backend", "claude-code", "--claude-bin", "/nonexistent/claude", dir}, "not found"},
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
	out, err := run(t, "judge", "--backend", "openai", "--model", "m", t.TempDir())
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
		{Preview: &report.PreviewInfo{Width: 9504, Height: 6320, Orientation: 8, Source: "tiff-ifd"}, FocusTarget: &report.FocusTarget{Source: "face", EyeSharpness: 0.1}},
		{Preview: &report.PreviewInfo{Width: 9504, Height: 6320, Orientation: 8, Source: "tiff-ifd"}, FocusTarget: &report.FocusTarget{Source: "none"}},
		{Preview: &report.PreviewInfo{Width: 1333, Height: 2000, Orientation: 1, Source: "exiftool:PreviewImage"}, FocusTarget: &report.FocusTarget{Source: "face"}},
		{Error: "preview: no JPEG"},
	}}
	want := "previews: long edge min/median/max = 2000/9504/9504 px; sources: exiftool:PreviewImage=1 tiff-ifd=2; orientation: 1=1 8=2; faces: 2/3, eyes measured on 1"
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
		if c.Name() == "judge" {
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
echo '{"type":"result","is_error":false,"structured_output":{"sharpness":{"score":2,"status":"missed_focus","focus_target":"x"},"exposure":{"score":7,"status":"good","ev_adjust":0,"clipping":"none","reason":""},"composition":{"score":6,"status":"good","issues":[],"crop":{"apply":false,"left":0,"top":0,"right":1,"bottom":1}},"people":{"present":true,"eyes":"open","expression":"good"},"notes":""},"usage":{"input_tokens":10,"output_tokens":5}}'
`

func TestCullMoveCulledThenRestoreEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	frame := filepath.Join(dir, "L1000001.DNG")
	tinyDNG(t, frame)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)

	out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--write-xmp", "--move-culled", dir)
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
	if !strings.Contains(out, "cull restore") {
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
		"bad eyes action":           {"judge", "--eyes-closed", "delete", dir},
		"bad outranked":             {"decide", "--outranked", "burn", dir},
		"bad raw action":            {"judge", "--raw-clipped", "maybe", dir},
		"batch needs anthropic":     {"judge", "--batch", "--backend", "openai", "--model", "m", dir},
		"batch with escalation":     {"judge", "--batch", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", dir},
		"bad raw threshold":         {"judge", "--raw-clip-threshold", "150", dir},
		"negative seq gap":          {"scan", "--seq-gap", "-1s", dir},
		"bad seq look, high":        {"scan", "--seq-look", "2", dir},
		"bad seq look, low":         {"scan", "--seq-look", "-0.1", dir},
		"negative keep-best":        {"judge", "--keep-best", "-1", dir},
		"keep-best too high":        {"decide", "--keep-best", "6", dir},
		"cull-max-sharpness >10":    {"decide", "--cull-max-sharpness", "11", dir},
		"second opinion with batch": {"judge", "--second-opinion", "--batch", dir},
		"unknown effort":            {"judge", "--effort", "turbo", dir},
		"decide overwrite alone":    {"decide", "--overwrite-xmp", dir},
		"bad escalate backend":      {"judge", "--escalate-backend", "gpt", "--escalate-model", "x", dir},
		"escalate needs model":      {"judge", "--escalate-backend", "anthropic", dir},
		"bad escalate-on":           {"judge", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", "--escalate-on", "blurry", dir},
		"negative sharp floor":      {"judge", "--review-below-sharpness", "-1", dir},
	} {
		if _, err := run(t, args...); err == nil || strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "unknown command") {
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
	out, err := run(t, "judge", "--estimate", dir)
	if err != nil || !strings.Contains(out, "estimate: 2 frames") || !strings.Contains(out, "$") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull-report.json")); err == nil {
		t.Fatal("--estimate must not run the pipeline")
	}
	out, err = run(t, "judge", "--estimate", "--backend", "openai", "--model", "m", dir)
	if err != nil || !strings.Contains(out, "no per-token cost") {
		t.Fatalf("openai estimate: err=%v\n%s", err, out)
	}
}

// --batch results are costed at half price; the summary must say so rather than
// "list price" (seen on the first live batch run).
func TestSummaryCostLabelNamesBatchPrice(t *testing.T) {
	rep := &report.Report{Results: []report.Result{{File: "a.DNG", Decision: "keep", CostUSD: 0.02}}}
	for _, tc := range []struct {
		batch bool
		want  string
	}{
		{false, "cost in report: $0.02 at list price\n"},
		{true, "cost in report: $0.02 at batch price (50%)\n"},
	} {
		cmd := NewRootCmd()
		var buf bytes.Buffer
		cmd.SetErr(&buf)
		printSummary(cmd, "r.json", rep, eval.Usage{}, "anthropic", tc.batch)
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("batch=%v: want %q in\n%s", tc.batch, tc.want, buf.String())
		}
	}
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "judge" {
			if u := c.Flags().Lookup("max-cost").Usage; !strings.Contains(u, "batch price with --batch") {
				t.Fatalf("--max-cost help must name the batch price: %q", u)
			}
		}
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
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
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
	out, err := run(t, "review", "--static", dir)
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull-review", "index.html")); err != nil || !strings.Contains(out, "index.html") {
		t.Fatalf("no sheet:\n%s", out)
	}
}

func TestCalibrateCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	rp := filepath.Join(dir, "cull-report.json")
	log := filepath.Join(dir, "cull-labels.jsonl")
	os.WriteFile(log, []byte(`{"file":"L1000001.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`+"\n"+
		`{"file":"X.DNG","label":"","stars":3,"at":"2026-09-27T20:00:01Z"}`+"\n"), 0o644)
	out, err := run(t, "calibrate", rp) // the log beside the report, by default
	if err != nil || !strings.Contains(out, "false-cull rate (keep → cull):   1/1") || !strings.Contains(out, "sweep") {
		t.Fatalf("calibrate: %v\n%s", err, out)
	}
	if strings.Contains(out, "not in the report") {
		t.Fatalf("a stars-only entry counted as a label:\n%s", out)
	}
	moved := filepath.Join(t.TempDir(), "mine.jsonl")
	os.Rename(log, moved)
	if out, err := run(t, "calibrate", "--labels", moved, rp); err != nil || !strings.Contains(out, "1/1") {
		t.Fatalf("explicit --labels: %v\n%s", err, out)
	}
	if _, err := run(t, "calibrate", rp); err == nil || !strings.Contains(err.Error(), "cull-labels.jsonl") {
		t.Fatalf("no log: %v", err)
	}
}

func TestRawClipFlagDefaults(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		want := map[string]string{"judge": "true", "scan": "false"}[c.Name()]
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
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	out, err := run(t, "apply-c1", dir)
	if err != nil || !strings.Contains(out, `tell application "Capture One"`) || !strings.Contains(out, `"L1000001.DNG"`) || !strings.Contains(out, "set color tag of v to 1") || strings.Contains(out, "set rating") {
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

func TestDecideLabelsMustExist(t *testing.T) {
	_, err := run(t, "decide", "--labels", filepath.Join(t.TempDir(), "nope.jsonl"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nope.jsonl") {
		t.Fatalf("missing labels log: %v", err)
	}
}

func TestApplyC1LabelsSetYourStars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	log := filepath.Join(dir, "cull-labels.jsonl")
	os.WriteFile(log, []byte(`{"file":"L1000001.DNG","label":"","stars":5,"at":"2026-09-27T20:00:00Z"}`+"\n"), 0o644)
	out, err := run(t, "apply-c1", "--labels", log, dir)
	if err != nil || !strings.Contains(out, "set rating of v to 5") {
		t.Fatalf("with labels: %v\n%s", err, out)
	}
	if out, err := run(t, "apply-c1", dir); err != nil || !strings.Contains(out, "set rating of v to 5") {
		t.Fatalf("the log beside the report is used by default: %v\n%s", err, out)
	}
	if out, _ := run(t, "apply-c1", "--no-labels", dir); strings.Contains(out, "set rating") {
		t.Fatalf("rating without your stars:\n%s", out)
	}
}

func TestCullHasNoCSVFlag(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "judge" && c.Flags().Lookup("csv") != nil {
			t.Fatal("cull --csv should be gone: the JSON report is the only run output")
		}
	}
}

func TestReviewServeFlagValidation(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"review", "--static", "--port", "8080", dir},
		{"review", "--static", "--no-open", dir},
		{"review", "--static", "--no-xmp", dir},
		{"review", "--static", "--overwrite-xmp", dir},
		{"review", "--no-xmp", "--overwrite-xmp", dir},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "with --") {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, gone := range []string{"--serve", "--open", "--write-xmp"} {
		if _, err := run(t, "review", gone, dir); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s should be gone (serving, opening and sidecars are the default): %v", gone, err)
		}
	}
}

func TestReviewServesAndWritesSidecarsByDefault(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	url, _, stop := startServe(t, dir)
	base, tok, ok := strings.Cut(url, "/#token=")
	if !ok || !strings.HasPrefix(base, "http://127.0.0.1:") || len(tok) != 32 {
		t.Fatalf("url %q", url)
	}
	req, _ := http.NewRequest("POST", base+"/api/labels", strings.NewReader(`{"file":"L1.DNG","label":"keep","stars":3}`))
	req.Header.Set("X-Cull-Token", tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("post: %v %v", err, res)
	}
	res.Body.Close()
	if err := stop(); err != nil {
		t.Fatalf("serve exit: %v", err)
	}
	if m, err := labels.Read(filepath.Join(dir, labels.FileName)); err != nil || m["L1.DNG"].Stars != 3 {
		t.Fatalf("log: %v %v", m, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L1.xmp")); !strings.Contains(string(b), `xmp:Rating="3"`) || !strings.Contains(string(b), `xmp:Label="Green"`) {
		t.Fatalf("sidecar not written by default:\n%s", b)
	}
}

// startServe runs `review` (serving, without opening a browser) until the returned
// stop is called, returning the printed URL and the lines printed before it.
func startServe(t *testing.T, args ...string) (url string, before []string, stop func() error) {
	t.Helper()
	pr, pw := io.Pipe()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(pw)
	cmd.SetArgs(append([]string{"review", "--no-open"}, args...))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx); pw.Close() }()
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if u, ok := strings.CutPrefix(sc.Text(), "review server: "); ok {
			url = u
			break
		}
		before = append(before, sc.Text())
	}
	go io.Copy(io.Discard, pr)
	return url, before, func() error { cancel(); return <-done }
}

func TestReviewDefaultPortIsStablePerReport(t *testing.T) {
	a, b := defaultPort("/shoot/a/cull-report.json"), defaultPort("/shoot/a/cull-report.json")
	c := defaultPort("/shoot/b/cull-report.json")
	if a != b || a < 49152 || a > 65535 || c < 49152 || c > 65535 {
		t.Fatalf("ports %d %d %d", a, b, c)
	}
}

func TestReviewServeKeepsItsPortAndFallsBackWhenBusy(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	want := fmt.Sprintf("http://127.0.0.1:%d/", defaultPort(filepath.Join(dir, "cull-report.json")))
	url, _, stop := startServe(t, dir)
	if !strings.HasPrefix(url, want) {
		t.Errorf("first run: %q, want the report's port %s", url, want)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	url2, _, stop := startServe(t, dir) // a restart gets the same origin, so the page's queue carries over
	if !strings.HasPrefix(url2, want) {
		t.Errorf("restart: %q, want %s", url2, want)
	}
	busy, before, stop2 := startServe(t, dir) // port taken by the running server
	if strings.HasPrefix(busy, want) || !strings.Contains(strings.Join(before, "\n"), "busy") {
		t.Errorf("busy port: %q, notes %q", busy, before)
	}
	stop2()
	stop()
}

// culledOne culls one tiny frame with the fake subscription backend (decision: cull).
func culledOne(t *testing.T) (dir, bin string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin = filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	return dir, bin
}

func TestDecideUsesYourLabelsByDefault(t *testing.T) {
	dir, _ := culledOne(t)
	os.WriteFile(filepath.Join(dir, labels.FileName), []byte(`{"file":"L1000001.DNG","label":"keep","stars":5,"at":"2026-09-27T20:00:00Z"}`+"\n"), 0o644)
	out, err := run(t, "decide", "--write-xmp", "--move-culled", dir)
	if err != nil || !strings.Contains(out, "using your labels") {
		t.Fatalf("decide: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1000001.DNG")); err != nil {
		t.Fatal("the frame you kept was moved")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L1000001.xmp")); !strings.Contains(string(b), `xmp:Rating="5"`) || !strings.Contains(string(b), `xmp:Label="Green"`) {
		t.Fatalf("your stars and verdict missing:\n%s", b)
	}
	if out, err := run(t, "decide", "--no-labels", "--write-xmp", "--move-culled", dir); err != nil {
		t.Fatalf("--no-labels: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "culled", "L1000001.DNG")); err != nil {
		t.Fatal("--no-labels should follow the model's cull")
	}
	if _, err := run(t, "decide", "--no-labels", "--labels", "x.jsonl", dir); err == nil {
		t.Fatal("--labels with --no-labels accepted")
	}
}

func TestCullResumeRespectsYourLabels(t *testing.T) {
	dir, bin := culledOne(t)
	os.WriteFile(filepath.Join(dir, labels.FileName), []byte(`{"file":"L1000001.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`+"\n"), 0o644)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--resume", "--move-culled", dir); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1000001.DNG")); err != nil {
		t.Fatal("cull --move-culled moved a frame you labeled keep")
	}
}

func TestToolIsCullAndModelStepIsJudge(t *testing.T) {
	root := NewRootCmd()
	if root.Name() != "cull" {
		t.Fatalf("root command %q", root.Name())
	}
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	if !names["judge"] || names["cull"] {
		t.Fatalf("subcommands %v: want judge, and no cull (it would read `cull cull`)", names)
	}
	if f := root.PersistentFlags().Lookup("report"); f == nil || !strings.Contains(f.Usage, "cull-report.json") {
		t.Fatalf("-o help: %+v", f)
	}
}

func TestOldCullSubcommandSuggestsJudge(t *testing.T) {
	if _, err := run(t, "cull", t.TempDir()); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("`cull cull` should point to judge: %v", err)
	}
}

// Task 10: --seq-gap/--seq-look replace --burst-gap/--burst-hash, and --outranked
// (with --keep-best) replaces --duplicates.
func TestOldSequenceAndDuplicatesFlagsAreGone(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"burst-gap":  {"scan", "--burst-gap", "2s", dir},
		"burst-hash": {"judge", "--burst-hash", "12", dir},
		"duplicates": {"decide", "--duplicates", "review", dir},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: want unknown flag, got %v", name, err)
		}
	}
}

func TestJudgeEstimateAddsApproximateRankingCost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	tinyDNG(t, filepath.Join(dir, "L2.DNG"))
	out, err := run(t, "judge", "--estimate", dir)
	if err != nil || !strings.Contains(out, "ranking ≈") || !strings.Contains(out, "if every frame lands in an 8-frame set") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if strings.Contains(out, "ranking ≤") {
		t.Fatalf("⌈n/8⌉ is not a bound: must not say ≤\n%s", out)
	}
	out, err = run(t, "judge", "--estimate", "--no-rank", dir)
	if err != nil || strings.Contains(out, "ranking ≈") {
		t.Fatalf("--no-rank must skip the ranking estimate: err=%v\n%s", err, out)
	}
}

// rankReportFixture writes a report with n rankable, identical (same look),
// tiny-DNG frames to dir/cull-report.json (schema_version 3, no look: as if
// from before sequence ranking) and returns its path.
func rankReportFixture(t *testing.T, dir, backend, model string, n int) string {
	t.Helper()
	sharp := &eval.Evaluation{Sharpness: eval.Sharpness{Score: 8, Status: "sharp"}, Exposure: eval.Exposure{Status: "good"},
		Composition: eval.Composition{Status: "good"}, People: eval.People{Present: true, Eyes: "open", Expression: "good"}}
	rep := &report.Report{SchemaVersion: 3, Backend: backend, Model: model, Dir: dir}
	for i := 0; i < n; i++ {
		f := filepath.Join(dir, fmt.Sprintf("L%d.DNG", i+1))
		tinyDNG(t, f)
		rep.Results = append(rep.Results, report.Result{File: f, Preview: &report.PreviewInfo{Orientation: 1}, Evaluation: sharp})
	}
	rp := filepath.Join(dir, "cull-report.json")
	if err := rep.Save(rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

func TestRankEstimateCountsSetsAndCalls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rp := rankReportFixture(t, dir, "", "", 2)

	out, err := run(t, "rank", "--estimate", dir)
	if err != nil || !strings.Contains(out, "1 set, 1 call") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	// The free look computation --estimate did (a v3 report) must be persisted,
	// exactly as Rank's own Ctrl-C path keeps looks computed so far.
	saved, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range saved.Results {
		if r.Look == "" {
			t.Fatalf("looks computed during --estimate must be saved: %+v", r)
		}
	}
	if saved.Sets != nil {
		t.Fatalf("--estimate must not write sets to the report: %+v", saved.Sets)
	}
}

func TestRankForceEstimateCountsAlreadyRankedSets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeRank), 0o755)

	if out, err := run(t, "rank", "--claude-bin", bin, dir); err != nil {
		t.Fatalf("rank: %v\n%s", err, out)
	}
	// Now fully ranked: without --force it needs no more ranking...
	out, err := run(t, "rank", "--estimate", dir)
	if err != nil || !strings.Contains(out, "0 sets, 0 calls") {
		t.Fatalf("already ranked, no --force: err=%v\n%s", err, out)
	}
	// ...but --force must still count it.
	out, err = run(t, "rank", "--force", "--estimate", dir)
	if err != nil || !strings.Contains(out, "1 set, 1 call") {
		t.Fatalf("--force --estimate must count the already-ranked set: err=%v\n%s", err, out)
	}
}

// A fake `claude` that always answers a rank call: frame 1 wins.
const fakeClaudeRank = `#!/bin/sh
cat > /dev/null
echo '{"type":"system","subtype":"init","apiKeySource":"none"}'
echo '{"type":"result","is_error":false,"structured_output":{"ranking":[{"frame":1,"strength":"sharper eyes","weakness":""},{"frame":2,"strength":"","weakness":"slightly softer"}],"summary":"frame 1 is the sharper take"},"usage":{"input_tokens":20,"output_tokens":10}}'
`

func TestRankCommandRanksAV3ReportEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rp := rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeRank), 0o755)

	// --keep-best 1: only the winner (L1, frame 1) should stay keep; the other
	// is outranked (default --outranked review).
	out, err := run(t, "rank", "--backend", "claude-code", "--claude-bin", bin, "--keep-best", "1", dir)
	if err != nil {
		t.Fatalf("rank: %v\n%s", err, out)
	}
	if strings.Contains(out, "report was judged with") {
		t.Fatalf("same backend/model as the report: no mismatch warning expected:\n%s", out)
	}
	got, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != report.SchemaVersion {
		t.Fatalf("schema version %d, want %d", got.SchemaVersion, report.SchemaVersion)
	}
	for _, r := range got.Results {
		if r.Look == "" {
			t.Fatalf("%s: no look computed", r.File)
		}
	}
	if len(got.Sets) != 1 || got.Sets[0].By != "model" || got.Sets[0].Of != 2 {
		t.Fatalf("sets: %+v", got.Sets)
	}
	if len(got.Sets[0].Order) != 2 {
		t.Fatalf("order: %+v", got.Sets[0].Order)
	}
	for _, r := range got.Results {
		switch filepath.Base(r.File) {
		case "L1.DNG":
			if r.Decision != eval.Keep {
				t.Errorf("L1 (rank 1, --keep-best 1): decision=%s, want keep", r.Decision)
			}
		case "L2.DNG":
			if r.Decision != eval.Review {
				t.Errorf("L2 (outranked): decision=%s, want review", r.Decision)
			}
		}
	}
}

func TestRankWarnsWhenBackendOrModelDiffersFromTheReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rankReportFixture(t, dir, "anthropic", "claude-sonnet-5", 2)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeRank), 0o755)

	want := "warning: report was judged with anthropic/claude-sonnet-5; ranking with claude-code/sonnet"

	// The warning must appear before an --estimate too, not only before a paid
	// run (--estimate only saves the report's looks, so the fixture is still
	// good for the real run below).
	out, err := run(t, "rank", "--backend", "claude-code", "--estimate", dir)
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("no mismatch warning on --estimate: %v\n%s", err, out)
	}

	out, err = run(t, "rank", "--backend", "claude-code", "--claude-bin", bin, dir)
	if err != nil {
		t.Fatalf("rank: %v\n%s", err, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("no mismatch warning:\n%s", out)
	}
}

func TestRankDefaultsBackendAndModelFromTheReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeRank), 0o755)

	// No --backend given: it must default to the report's claude-code, not
	// anthropic (which would fail here with no API key).
	out, err := run(t, "rank", "--claude-bin", bin, dir)
	if err != nil {
		t.Fatalf("rank: %v\n%s", err, out)
	}
	if !strings.Contains(out, "backend: claude-code") {
		t.Fatalf("did not default to the report's backend:\n%s", out)
	}
}

func TestRankAppliesBackendDefaultConcurrency(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "rank" {
			if f := c.Flags().Lookup("concurrency"); f == nil || f.DefValue != "0" {
				t.Fatalf("rank --concurrency flag: %+v", f)
			}
			return
		}
	}
	t.Fatal("no rank command")
}

// --batch needs anthropic even when the report was judged with another backend
// and --backend wasn't given: an explicit non-anthropic --backend must still
// fail, with judge's own text.
func TestRankBatchRejectsNonAnthropicBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	out, err := run(t, "rank", "--batch", "--backend", "openai", "--model", "m", dir)
	if err == nil || !strings.Contains(err.Error(), "--batch uses the Message Batches API: --backend anthropic only") {
		t.Fatalf("got %v", err)
	}
	// The mismatch warning (report judged with claude-code, ranking with
	// openai) must not print before this error: it's about to refuse anyway.
	if strings.Contains(out, "report was judged with") {
		t.Fatalf("must not print the mismatch warning before erroring:\n%s", out)
	}
}

// --batch --estimate on a report judged with claude-code must still use
// anthropic (its model default, since the report's backend doesn't match) and
// price at the batch rate, needing no key and calling no model.
func TestRankBatchEstimateUsesAnthropicAndBatchPrice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	out, err := run(t, "rank", "--batch", "--estimate", dir)
	if err != nil || !strings.Contains(out, "batch price (50%)") || !strings.Contains(out, "claude-sonnet-5") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

// After a claude-code judge, rank --batch moves to anthropic (the API): the
// mismatch warning this exists for ("report was judged with X; ranking with
// Y") must print for that move too, not just for an explicitly-chosen backend
// — and it must appear only AFTER the --batch override decides the real
// backend/model, not the report-based default that --batch then overrides.
func TestRankBatchWarnsAfterOverrideForClaudeCodeReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	rankReportFixture(t, dir, "claude-code", "sonnet", 2)
	out, err := run(t, "rank", "--batch", "--estimate", dir)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	want := "warning: report was judged with claude-code/sonnet; ranking with anthropic/claude-sonnet-5-5"
	if !strings.Contains(out, want) {
		t.Fatalf("no mismatch warning after the --batch override:\n%s", out)
	}
}

// applyBackendModel is rank's cfg-building extracted so it's testable without
// cobra or a real backend/network call: it must set cfg.Backend/cfg.Model to
// what rank ends up using (Important #1 — 'cull rank --batch' previously left
// them "", so its rank-batch state recorded "" / "" and couldn't re-attach to
// judge --batch's own state, or the reverse, and the model-mismatch guard was
// disabled for rank).
func TestRankApplyBackendModelSetsCfgBackendAndModel(t *testing.T) {
	o := &rankOpts{}
	o.batch = true
	var cfg pipeline.Config
	rep := &report.Report{Backend: "claude-code", Model: "sonnet"}
	warning, err := o.applyBackendModel(&cfg, false, false, rep)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "anthropic" || cfg.Model != "claude-sonnet-5-5" {
		t.Fatalf("cfg not set to what rank actually uses: %+v", cfg)
	}
	want := "report was judged with claude-code/sonnet; ranking with anthropic/claude-sonnet-5-5"
	if warning != want {
		t.Fatalf("warning=%q, want %q", warning, want)
	}
}

// An explicit non-anthropic --backend with --batch errors before cfg or the
// warning are touched.
func TestRankApplyBackendModelRejectsExplicitNonAnthropicBatch(t *testing.T) {
	o := &rankOpts{}
	o.backend, o.batch = "openai", true
	var cfg pipeline.Config
	rep := &report.Report{Backend: "claude-code", Model: "sonnet"}
	warning, err := o.applyBackendModel(&cfg, true, false, rep)
	if err == nil || !strings.Contains(err.Error(), "--batch uses the Message Batches API: --backend anthropic only") {
		t.Fatalf("got %v", err)
	}
	if warning != "" {
		t.Fatalf("no warning expected before the batch check errors, got %q", warning)
	}
	if cfg.Backend != "" || cfg.Model != "" {
		t.Fatalf("cfg must be untouched on error: %+v", cfg)
	}
}

// Without --batch, applyBackendModel still sets cfg.Backend/cfg.Model to what
// a sync rank uses (matching judge's own cfg.Backend/cfg.Model, cull.go).
func TestRankApplyBackendModelSetsCfgForSyncRank(t *testing.T) {
	o := &rankOpts{}
	var cfg pipeline.Config
	rep := &report.Report{Backend: "claude-code", Model: "sonnet"}
	if _, err := o.applyBackendModel(&cfg, false, false, rep); err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "claude-code" || cfg.Model != "sonnet" {
		t.Fatalf("cfg: %+v", cfg)
	}
}

// Sync cull rank refuses before RankCalls/fillLooks while a rank-batch file
// exists: a v3 report's looks stay uncomputed (that decode pass is skipped for
// a run about to refuse anyway).
func TestRankRefusesBeforeFillLooksAtCLILevelWhileRankBatchPending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rp := rankReportFixture(t, dir, "", "", 2) // v3, no looks
	if err := os.WriteFile(rp+".rank-batch.json", []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, "rank", "--estimate", dir)
	if err == nil || !strings.Contains(err.Error(), rp+".rank-batch.json") {
		t.Fatalf("got %v", err)
	}
	saved, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range saved.Results {
		if r.Look != "" {
			t.Fatalf("looks must not be computed before the refusal: %+v", r)
		}
	}
}

func TestKeepBestTooHighExplainsWhy(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, "judge", "--keep-best", "6", dir)
	if err == nil || !strings.Contains(err.Error(), "8-frames-per-call") {
		t.Fatalf("want an explanation mentioning the 8-frames-per-call limit, got %v", err)
	}
}

// A report remembers the policy its decisions came from, so a later decide (or rank,
// or calibrate) without the flags keeps that tuning instead of silently resetting it
// to the defaults (seen live: rank after `decide --review-below-sharpness 7` undid it).
// A flag typed on the command line still wins, field by field.
func TestDecideReusesTheReportsPolicyUnlessAFlagOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("judge: %v\n%s", err, out)
	}
	rp := filepath.Join(dir, "cull-report.json")
	policy := func() eval.Policy {
		t.Helper()
		rep, err := report.Load(rp)
		if err != nil || rep.Policy == nil {
			t.Fatalf("report policy: %v, %+v", err, rep)
		}
		return *rep.Policy
	}
	if out, err := run(t, "decide", "--review-below-sharpness", "7", "--eyes-closed", "cull", "--keep-best", "2", dir); err != nil {
		t.Fatalf("decide: %v\n%s", err, out)
	}
	out, err := run(t, "decide", dir)
	if err != nil {
		t.Fatalf("decide: %v\n%s", err, out)
	}
	if p := policy(); p.ReviewBelowSharpness != 7 || p.EyesClosed != eval.ActionCull || p.KeepBest != 2 {
		t.Fatalf("plain decide reset the stored policy: %+v", p)
	}
	for _, want := range []string{"--review-below-sharpness 7", "--eyes-closed cull", "--keep-best 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the reused setting %q isn't named:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--min-crop-area") {
		t.Errorf("a setting still at its default is named:\n%s", out)
	}
	if out, err := run(t, "decide", "--review-below-sharpness", "0", dir); err != nil {
		t.Fatalf("decide: %v\n%s", err, out)
	}
	if p := policy(); p.ReviewBelowSharpness != 0 || p.EyesClosed != eval.ActionCull || p.KeepBest != 2 {
		t.Fatalf("a typed flag must override only its own field: %+v", p)
	}
}

// Flags without a stored policy behave as before; a stored policy that is invalid
// (hand-edited) is refused rather than applied.
func TestPolicyResolve(t *testing.T) {
	var pf policyFlags
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	pf.register(fs)
	p, notes, err := pf.resolve(fs, nil)
	if err != nil || p.KeepBest != 3 || p.Outranked != eval.ActionReview || len(notes) != 0 {
		t.Fatalf("defaults: %+v %v %v", p, notes, err)
	}
	saved := p
	saved.RawClipThreshold = 2
	if err := fs.Parse([]string{"--raw-clipped", "cull"}); err != nil {
		t.Fatal(err)
	}
	p, notes, err = pf.resolve(fs, &saved)
	if err != nil || p.RawClipThreshold != 2 || p.RawClipped != eval.ActionCull {
		t.Fatalf("merge: %+v %v", p, err)
	}
	if len(notes) != 1 || notes[0] != "--raw-clip-threshold 2" {
		t.Fatalf("notes: %q", notes)
	}
	bad := saved
	bad.KeepBest = 9
	var fresh policyFlags
	fs2 := pflag.NewFlagSet("u", pflag.ContinueOnError)
	fresh.register(fs2)
	if _, _, err := fresh.resolve(fs2, &bad); err == nil || !strings.Contains(err.Error(), "keep-best") {
		t.Fatalf("an invalid stored policy must be refused: %v", err)
	}
}

// judge --resume continues a report, so it keeps the report's policy too.
func TestJudgeResumeKeepsTheReportsPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	judge := []string{"judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off"}
	if out, err := run(t, append(judge, dir)...); err != nil {
		t.Fatalf("judge: %v\n%s", err, out)
	}
	if out, err := run(t, "decide", "--eyes-closed", "cull", dir); err != nil {
		t.Fatalf("decide: %v\n%s", err, out)
	}
	out, err := run(t, append(judge, "--resume", dir)...)
	if err != nil {
		t.Fatalf("judge --resume: %v\n%s", err, out)
	}
	rep, err := report.Load(filepath.Join(dir, "cull-report.json"))
	if err != nil || rep.Policy == nil || rep.Policy.EyesClosed != eval.ActionCull {
		t.Fatalf("judge --resume reset the stored policy: %v %+v\n%s", err, rep.Policy, out)
	}
	if !strings.Contains(out, "--eyes-closed cull") {
		t.Fatalf("the reused setting isn't named:\n%s", out)
	}
}

func TestScanRefusesJudgedReportUnlessFresh(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	judged := &report.Report{SchemaVersion: report.SchemaVersion, Backend: "anthropic", Model: "m",
		Results: []report.Result{{File: filepath.Join(dir, "L1.DNG"), Evaluation: &eval.Evaluation{}, CostUSD: 0.02}}}
	if err := judged.Save(filepath.Join(dir, "cull-report.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "scan", dir); err == nil || !strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("scan over a judged report: got %v", err)
	}
	if out, err := run(t, "scan", "--fresh", dir); err != nil {
		t.Fatalf("scan --fresh: %v\n%s", err, out)
	}
}

func TestJudgeRefusesMaxCostForUnpricedModel(t *testing.T) {
	dir := t.TempDir() // no DNGs or key needed: the refusal comes first
	_, err := run(t, "judge", "--model", "claude-unknown-9", "--max-cost", "5", dir)
	if err == nil || !strings.Contains(err.Error(), "no price") {
		t.Fatalf("got %v", err)
	}
}

func TestRankRefusesMaxCostForUnpricedModel(t *testing.T) {
	dir := t.TempDir()
	rankReportFixture(t, dir, "anthropic", "claude-unknown-9", 2)
	_, err := run(t, "rank", "--max-cost", "5", dir)
	if err == nil || !strings.Contains(err.Error(), "no price") {
		t.Fatalf("got %v", err)
	}
}

func TestJudgeDefaultsToCurrentSonnet(t *testing.T) {
	if got := backendDefaults["anthropic"].model; got != "claude-sonnet-5-5" {
		t.Fatalf("default %q", got)
	}
}

func TestJudgeRefusesMaxCostForUnpricedEscalationModel(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, "judge", "--escalate-backend", "anthropic", "--escalate-model", "claude-unknown-9", "--max-cost", "5", dir)
	if err == nil || !strings.Contains(err.Error(), "no price") {
		t.Fatalf("got %v", err)
	}
}

// A stored --seq-look is reused unless typed again.
func TestResolveSeqReusesStoredSettings(t *testing.T) {
	fs := NewRootCmd().PersistentFlags()
	fs.Parse(nil)
	stored := &report.Sequences{GapSeconds: 60, Look: 0.12}
	got, notes := resolveSeq(fs, group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}, stored)
	if got.MaxLook != 0.12 || got.Gap != time.Minute || len(notes) != 1 || notes[0] != "--seq-look 0.12" {
		t.Fatalf("got %+v notes %v", got, notes)
	}
	fs.Parse([]string{"--seq-look", "0.05"})
	if got, _ := resolveSeq(fs, group.Options{Gap: time.Minute, MaxLook: 0.05}, stored); got.MaxLook != 0.05 {
		t.Fatalf("typed flag didn't win: %+v", got)
	}
}

func TestDecideWithoutStoredSequencesUsesFlags(t *testing.T) {
	fs := NewRootCmd().PersistentFlags()
	fs.Parse(nil)
	cur := group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	if got, notes := resolveSeq(fs, cur, nil); got != cur || len(notes) != 0 {
		t.Fatalf("got %+v %v", got, notes)
	}
}

func TestDecideNamesStoredSequenceSettings(t *testing.T) {
	dir := t.TempDir()
	rp := rankReportFixture(t, dir, "anthropic", "claude-sonnet-5", 2)
	rep, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	rep.Seq = &report.Sequences{GapSeconds: 60, Look: 0.5}
	rep.Save(rp)
	out, err := run(t, "decide", dir)
	if err != nil || !strings.Contains(out, "--seq-look 0.5") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	rep, _ = report.Load(rp)
	if rep.Seq == nil || rep.Seq.Look != 0.5 {
		t.Fatalf("stored grouping lost: %+v", rep.Seq)
	}
}

func TestCalibrateRefusesDuplicateNamesWithLabels(t *testing.T) {
	dir := t.TempDir()
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: "anthropic", Model: "m", Results: []report.Result{
		{File: filepath.Join(dir, "a", "L1.DNG"), Evaluation: &eval.Evaluation{}, Decision: eval.Keep},
		{File: filepath.Join(dir, "b", "L1.DNG"), Evaluation: &eval.Evaluation{}, Decision: eval.Cull}}}
	rp := filepath.Join(dir, "cull-report.json")
	rep.Save(rp)
	labels.Append(filepath.Join(dir, labels.FileName), labels.Entry{File: "L1.DNG", Label: "keep"})
	if _, err := run(t, "calibrate", rp); err == nil || !strings.Contains(err.Error(), "share a file name") {
		t.Fatalf("got %v", err)
	}
}

func TestCalibrateCompareNeedsNoLabels(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, d eval.Decision) string {
		rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: "anthropic", Model: "m", Results: []report.Result{
			{File: filepath.Join(dir, "L1.DNG"), Evaluation: &eval.Evaluation{}, Decision: d}}}
		p := filepath.Join(dir, name)
		rep.Save(p)
		return p
	}
	a, b := mk("a.json", eval.Cull), mk("b.json", eval.Review)
	out, err := run(t, "calibrate", "--compare", a, b)
	if err != nil || !strings.Contains(out, "agree on 0/1") || !strings.Contains(out, "cull→review 1") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if _, err := run(t, "calibrate", "--compare", a); err == nil {
		t.Fatal("--compare with one report must fail")
	}
}

func TestCullMaxSharpnessDefault(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "decide" {
			// The prompt's missed_focus band ends at 2.9; a 3 is soft-band, so it must not cull.
			if f := c.Flags().Lookup("cull-max-sharpness"); f == nil || f.DefValue != "2.9" {
				t.Fatalf("flag: %+v", f)
			}
			return
		}
	}
	t.Fatal("no decide command")
}

func TestSecondOpinionRefusedWithBatch(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "judge", "--second-opinion", "--batch", dir); err == nil || !strings.Contains(err.Error(), "synchronous") {
		t.Fatalf("got %v", err)
	}
}

func TestRankTwiceFlagOnJudgeAndRank(t *testing.T) {
	seen := 0
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "judge" || c.Name() == "rank" {
			if f := c.Flags().Lookup("rank-twice"); f == nil || f.DefValue != "false" {
				t.Fatalf("%s: rank-twice %+v", c.Name(), f)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("checked %d commands", seen)
	}
}

func TestRankEstimateCountsReversedCalls(t *testing.T) {
	dir := t.TempDir()
	rankReportFixture(t, dir, "anthropic", "claude-sonnet-5", 2)
	once, err := run(t, "rank", "--estimate", dir)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := run(t, "rank", "--estimate", "--rank-twice", dir)
	if err != nil || once == twice {
		t.Fatalf("estimate unchanged by --rank-twice:\n%s\n%s", once, twice)
	}
}

func TestEffortRefusedOnOpenAI(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "judge", "--backend", "openai", "--model", "m", "--effort", "low", dir); err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("got %v", err)
	}
}

// Rankings in one report can't mix efforts: rank uses the report's, and refuses another.
func TestRankRefusesADifferentEffort(t *testing.T) {
	dir := t.TempDir()
	rankReportFixture(t, dir, "anthropic", "claude-sonnet-5", 2)
	if _, err := run(t, "rank", "--estimate", "--effort", "low", dir); err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("got %v", err)
	}
	if _, err := run(t, "rank", "--estimate", "--locate-effort", "low", dir); err == nil || !strings.Contains(err.Error(), "locate") {
		t.Fatalf("got %v", err)
	}
}

// 1024 px cut frame input tokens 16% with verdicts within run-to-run noise
// (CLAUDE.md, 2026-10-01 cost A/B); the subject crop carries the sharpness call.
func TestMaxEdgeDefault(t *testing.T) {
	if f := NewRootCmd().PersistentFlags().Lookup("max-edge"); f == nil || f.DefValue != "1024" {
		t.Fatalf("max-edge flag: %+v", f)
	}
}

func TestLabelsFlagOnJudgeAndReview(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if (c.Name() == "judge" || c.Name() == "review") && c.Flags().Lookup("labels") == nil {
			t.Errorf("%s has no --labels", c.Name())
		}
	}
}

func TestApplyC1ProbeNeedsNoDir(t *testing.T) {
	out, err := run(t, "apply-c1", "--probe")
	if err != nil || !strings.Contains(out, "tell application") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if _, err := run(t, "apply-c1"); err == nil {
		t.Fatal("apply-c1 without --probe needs a folder")
	}
}

func TestDecideOverwriteNeedsWriteXMP(t *testing.T) {
	if _, err := run(t, "decide", "--overwrite-xmp", t.TempDir()); err == nil || !strings.Contains(err.Error(), "--write-xmp") {
		t.Fatalf("got %v", err)
	}
}
