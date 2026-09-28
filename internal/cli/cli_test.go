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

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/labels"
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
		"xmp-develop without write-xmp":   {"judge", "--xmp-develop", dir},
		"overwrite-xmp without write-xmp": {"judge", "--overwrite-xmp", dir},
		"bad min-crop-area":               {"judge", "--min-crop-area", "1.5", dir},
		"missing dir arg":                 {"judge"},
		"not a directory":                 {"scan", dir + "/nope"},
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
echo '{"type":"result","is_error":false,"structured_output":{"sharpness":{"score":2,"status":"missed_focus","focus_target":"x"},"exposure":{"score":7,"status":"good","ev_adjust":0,"clipping":"none","reason":""},"composition":{"score":6,"status":"good","issues":[],"crop":{"apply":false,"left":0,"top":0,"right":1,"bottom":1},"straighten_degrees":0},"people":{"present":true,"eyes":"open","expression":"good"},"notes":""},"usage":{"input_tokens":10,"output_tokens":5}}'
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
		"bad eyes action":       {"judge", "--eyes-closed", "delete", dir},
		"bad duplicates":        {"decide", "--duplicates", "burn", dir},
		"bad raw action":        {"judge", "--raw-clipped", "maybe", dir},
		"batch needs anthropic": {"judge", "--batch", "--backend", "openai", "--model", "m", dir},
		"batch with escalation": {"judge", "--batch", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", dir},
		"bad raw threshold":     {"judge", "--raw-clip-threshold", "150", dir},
		"negative burst gap":    {"scan", "--burst-gap", "-1s", dir},
		"bad escalate backend":  {"judge", "--escalate-backend", "gpt", "--escalate-model", "x", dir},
		"escalate needs model":  {"judge", "--escalate-backend", "anthropic", dir},
		"bad escalate-on":       {"judge", "--escalate-backend", "anthropic", "--escalate-model", "claude-opus-5", "--escalate-on", "blurry", dir},
		"negative sharp floor":  {"judge", "--review-below-sharpness", "-1", dir},
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
