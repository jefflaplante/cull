package calib

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
)

func TestCompareRunsCountsFlips(t *testing.T) {
	mk := func(d eval.Decision, status string, score float64) report.Result {
		return report.Result{Decision: d, Evaluation: &eval.Evaluation{Sharpness: eval.Sharpness{Status: status, Score: score}}}
	}
	a := &report.Report{Results: []report.Result{mk(eval.Keep, "sharp", 8), mk(eval.Cull, "missed_focus", 2), mk(eval.Review, "soft", 5), mk(eval.Keep, "sharp", 9)}}
	b := &report.Report{Results: []report.Result{mk(eval.Keep, "acceptable", 7), mk(eval.Review, "soft", 4), mk(eval.Review, "soft", 5)}}
	names := []string{"A.DNG", "B.DNG", "C.DNG", "D.DNG"}
	for i := range a.Results {
		a.Results[i].File = "/x/" + names[i]
	}
	for i := range b.Results {
		b.Results[i].File = "/y/" + names[i] // runs are matched by base name
	}
	d, err := CompareRuns(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if d.N != 3 || d.Same != 2 || d.Flips["cull→review"] != 1 || d.StatusFlips != 2 || d.Missing != 1 {
		t.Fatalf("%+v", d)
	}
	if math.Abs(d.MeanAbsSharpDelta-1) > 1e-9 { // |8-7|, |2-4|, |5-5|: (1+2+0)/3
		t.Fatalf("mean delta %v", d.MeanAbsSharpDelta)
	}
	var out bytes.Buffer
	FormatRunDiff(&out, "a.json", "b.json", d)
	for _, want := range []string{"3 frames in both", "agree on 2/3", "cull→review 1", "sharpness status differs on 2", "1 frame(s) assessed in only one"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCompareRunsCallsOutCrossings(t *testing.T) {
	a := &report.Report{Results: []report.Result{{File: "/x/A.DNG", Decision: eval.Keep, Evaluation: &eval.Evaluation{}}}}
	b := &report.Report{Results: []report.Result{{File: "/x/A.DNG", Decision: eval.Cull, Evaluation: &eval.Evaluation{}}}}
	var out bytes.Buffer
	d, _ := CompareRuns(a, b)
	FormatRunDiff(&out, "a", "b", d)
	if !strings.Contains(out.String(), "keep↔cull crossings: 1") {
		t.Fatalf("%s", out.String())
	}
}

// Two frames sharing a base name (a recursive shoot) can't be matched by name:
// refuse rather than count one of them twice or not at all.
func TestCompareRunsRefusesDuplicateNames(t *testing.T) {
	r := func(p string) report.Result {
		return report.Result{File: p, Decision: eval.Keep, Evaluation: &eval.Evaluation{}}
	}
	a := &report.Report{Results: []report.Result{r("/s/a/L1.DNG"), r("/s/b/L1.DNG")}}
	b := &report.Report{Results: []report.Result{r("/s/a/L1.DNG")}}
	if _, err := CompareRuns(a, b); err == nil || !strings.Contains(err.Error(), "share a file name") {
		t.Fatalf("got %v", err)
	}
}
