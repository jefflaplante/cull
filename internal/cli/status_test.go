package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
)

func TestStatusWithoutReport(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	out, err := run(t, "status", dir)
	if err != nil || !strings.Contains(out, "1 DNG") || !strings.Contains(out, "next: cull scan") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func statusShoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var results []report.Result
	for _, n := range []struct {
		name string
		d    eval.Decision
	}{{"L1", eval.Keep}, {"L2", eval.Review}, {"L3", eval.Cull}} {
		p := filepath.Join(dir, n.name+".DNG")
		tinyDNG(t, p)
		st, _ := os.Stat(p)
		results = append(results, report.Result{File: p, Size: st.Size(), ModTime: st.ModTime(), Decision: n.d, Evaluation: &eval.Evaluation{}})
	}
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: "claude-code", Model: "sonnet", Dir: dir, Results: results}
	if err := rep.Save(filepath.Join(dir, "cull-report.json")); err != nil {
		t.Fatal(err)
	}
	labels.Append(filepath.Join(dir, labels.FileName), labels.Entry{File: "L1.DNG", Label: "keep", Stars: 3})
	return dir
}

func TestStatusCountsAndNextStep(t *testing.T) {
	dir := statusShoot(t)
	tinyDNG(t, filepath.Join(dir, "L4.DNG")) // new since judging
	out, err := run(t, "status", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"4 DNGs", "assessed 3", "not yet judged 1", "keep 1 · review 1 · cull 1", "labelled 1/3", "rated 1", "next: cull judge --resume"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusSuggestsReviewThenCalibrate(t *testing.T) {
	dir := statusShoot(t)
	if out, _ := run(t, "status", dir); !strings.Contains(out, "next: label a sample in cull review") {
		t.Fatalf("%s", out)
	}
}

func TestStatusSeesPendingBatch(t *testing.T) {
	dir := statusShoot(t)
	os.WriteFile(filepath.Join(dir, "cull-report.json.batch.json"), []byte("{}"), 0o644)
	if out, _ := run(t, "status", dir); !strings.Contains(out, "--batch --resume") {
		t.Fatalf("%s", out)
	}
}

// With -o, the suggested command keeps the same report.
func TestStatusNextKeepsReportPath(t *testing.T) {
	dir := statusShoot(t)
	other := filepath.Join(t.TempDir(), "run.json")
	os.Rename(filepath.Join(dir, "cull-report.json"), other)
	if out, _ := run(t, "status", "-o", other, dir); !strings.Contains(out, "-o "+other) {
		t.Fatalf("%s", out)
	}
}
