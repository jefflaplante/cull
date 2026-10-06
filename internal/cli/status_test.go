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
	for _, want := range []string{"4 DNGs", "assessed 3", "not yet judged 1", "keep 1 · review 1 · cull 1", "labelled 1/3", "rated 1", "next: cull judge "} {
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
	if out, _ := run(t, "status", dir); !strings.Contains(out, "cull judge --batch ") {
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

func TestImportLabelsAppendsToTheLog(t *testing.T) {
	dir := statusShoot(t) // L1 already labelled keep
	export := filepath.Join(t.TempDir(), "cull-labels.jsonl")
	os.WriteFile(export, []byte(`{"file":"L2.DNG","label":"cull","stars":0,"at":"2026-10-01T10:00:00Z"}
{"file":"L3.DNG","label":"","stars":4,"at":"2026-10-01T10:00:01Z"}
`), 0o644)
	out, err := run(t, "import-labels", export, dir)
	if err != nil || !strings.Contains(out, "imported 2") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	got, _ := labels.Read(filepath.Join(dir, labels.FileName))
	if got["L1.DNG"].Label != "keep" || got["L2.DNG"].Label != "cull" || got["L3.DNG"].Stars != 4 {
		t.Fatalf("%+v", got)
	}
}

// Labels for frames the report doesn't hold come from another shoot's sheet.
func TestImportLabelsRefusesStrangers(t *testing.T) {
	dir := statusShoot(t)
	export := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(export, []byte(`{"file":"L2.DNG","label":"cull","stars":0,"at":"2026-10-01T10:00:00Z"}
{"file":"OTHER.DNG","label":"keep","stars":0,"at":"2026-10-01T10:00:00Z"}
`), 0o644)
	if _, err := run(t, "import-labels", export, dir); err == nil || !strings.Contains(err.Error(), "OTHER.DNG") {
		t.Fatalf("got %v", err)
	}
	if got, _ := labels.Read(filepath.Join(dir, labels.FileName)); got["L2.DNG"].Label != "" {
		t.Fatal("imported part of a refused export")
	}
}

// Suggested commands quote paths, so a shoot folder with spaces can be pasted as is.
func TestStatusQuotesPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-02 Smith wedding")
	os.MkdirAll(dir, 0o755)
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	out, err := run(t, "status", dir)
	if err != nil || !strings.Contains(out, "next: cull scan '"+dir+"'") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	rep := filepath.Join(t.TempDir(), "it's.json")
	out, _ = run(t, "status", "-o", rep, dir)
	if !strings.Contains(out, `-o '`+strings.ReplaceAll(rep, "'", `'\''`)+`' '`+dir+`'`) {
		t.Fatalf("report path not quoted:\n%s", out)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/a/Pictures/2026-10-02": "/Users/a/Pictures/2026-10-02",
		"/a/b c":                       "'/a/b c'",
		"/a/$HOME":                     "'/a/$HOME'",
		"/a/it's":                      `'/a/it'\''s'`,
		"":                             "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// Continuing a report (the default), the estimate prices only the frames still to
// judge, not the folder; --fresh prices them all.
func TestEstimateOnResumeCountsOnlyWhatsLeft(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := statusShoot(t) // 3 judged
	tinyDNG(t, filepath.Join(dir, "L4.DNG"))
	out, err := run(t, "judge", "--estimate", "--backend", "claude-code", dir)
	if err != nil || !strings.Contains(out, "estimate: 1 frames") || !strings.Contains(out, "3 already judged") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	out, err = run(t, "judge", "--estimate", "--fresh", "--backend", "claude-code", dir)
	if err != nil || !strings.Contains(out, "estimate: 4 frames") {
		t.Fatalf("with --fresh: err=%v\n%s", err, out)
	}
}

// moveTo moves a shoot's frame into a sub-folder and records it, as --sort does.
func moveTo(t *testing.T, dir, name, sub string) {
	t.Helper()
	rp := filepath.Join(dir, "cull-report.json")
	rep, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, sub), 0o755)
	for i := range rep.Results {
		r := &rep.Results[i]
		if filepath.Base(r.File) == name {
			dst := filepath.Join(dir, sub, name)
			if err := os.Rename(r.File, dst); err != nil {
				t.Fatal(err)
			}
			r.MovedTo = dst
		}
	}
	rep.Save(rp)
}

func labelAll(t *testing.T, dir string) {
	t.Helper()
	for _, n := range []string{"L1.DNG", "L2.DNG", "L3.DNG"} {
		labels.Append(filepath.Join(dir, labels.FileName), labels.Entry{File: n, Label: "keep"})
	}
}

// A sorted shoot counts its sorted frames and suggests --sort, not --sort=culls;
// a shoot smaller than the label sample, fully labelled, still gets that far.
func TestStatusAfterSort(t *testing.T) {
	dir := statusShoot(t)
	labelAll(t, dir)
	moveTo(t, dir, "L1.DNG", "keep")
	moveTo(t, dir, "L2.DNG", "review")
	moveTo(t, dir, "L3.DNG", "cull")
	out, err := run(t, "status", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{": 3 DNGs (3 sorted into folders)", "decide --sort"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--sort=culls") {
		t.Errorf("suggests --sort=culls for a sorted shoot:\n%s", out)
	}
}

// A shoot whose only moved frames are culls (cull/, or the old culled/) suggests
// --sort=culls and counts them.
func TestStatusAfterSortCulls(t *testing.T) {
	for _, folder := range []string{"cull", "culled"} {
		dir := statusShoot(t)
		labelAll(t, dir)
		moveTo(t, dir, "L3.DNG", folder)
		out, _ := run(t, "status", dir)
		for _, want := range []string{"(1 sorted into folders)", "decide --sort=culls"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s/: lacks %q:\n%s", folder, want, out)
			}
		}
	}
}

func TestStatusNothingMovedSuggestsSort(t *testing.T) {
	dir := statusShoot(t)
	labelAll(t, dir)
	if out, _ := run(t, "status", dir); !strings.Contains(out, "decide --sort") {
		t.Errorf("%s", out)
	}
}

// A judged report with an unranked multi-frame set points at judge, not rank.
func TestStatusUnrankedSetPointsAtJudge(t *testing.T) {
	dir := statusShoot(t)
	path := filepath.Join(dir, "cull-report.json")
	rep, err := report.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rep.Sets = []report.Set{{ID: 1, Of: 2, By: "scores", Members: []string{rep.Results[0].File, rep.Results[1].File}}}
	if err := rep.Save(path); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "status", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cull judge --estimate", "unranked set"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cull rank") {
		t.Errorf("mentions cull rank:\n%s", out)
	}
}

// A recorded ranking batch is re-attached with judge --batch.
func TestStatusRankBatchPointsAtJudge(t *testing.T) {
	dir := statusShoot(t)
	os.WriteFile(filepath.Join(dir, "cull-report.json.rank-batch.json"), []byte("{}"), 0o644)
	out, err := run(t, "status", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next: cull judge --batch") || strings.Contains(out, "cull rank") {
		t.Fatalf("%s", out)
	}
}
