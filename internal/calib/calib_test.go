package calib

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
)

func frame(name string, d eval.Decision, score float64) report.Result {
	return report.Result{File: "/shoot/" + name, Decision: d,
		Evaluation: &eval.Evaluation{Sharpness: eval.Sharpness{Status: "sharp", Score: score}}}
}

func testReport() *report.Report {
	return &report.Report{Backend: "anthropic", Model: "claude-sonnet-5", Results: []report.Result{
		frame("A.DNG", eval.Keep, 9), frame("B.DNG", eval.Cull, 2), frame("C.DNG", eval.Keep, 5),
		frame("D.DNG", eval.Review, 6), {File: "/shoot/E.DNG"}, // E: scan-only, no decision
	}}
}

func TestCompareRatesAndMissing(t *testing.T) {
	labels := map[string]string{"A.DNG": "keep", "B.DNG": "keep", "C.DNG": "cull", "D.DNG": "review", "E.DNG": "keep", "Z.DNG": "cull"}
	m := Compare(testReport(), labels)
	if m.N != 4 || m.Missing != 2 {
		t.Fatalf("n=%d missing=%d", m.N, m.Missing)
	}
	if m.Counts["keep"]["cull"] != 1 || m.Counts["cull"]["keep"] != 1 || m.Counts["review"]["review"] != 1 {
		t.Fatalf("counts %v", m.Counts)
	}
	fc, mc, rr := m.Rates()
	if fc != 0.5 || mc != 1 || rr != 0.25 {
		t.Fatalf("rates false-cull=%v missed-cull=%v review=%v", fc, mc, rr)
	}
	var out bytes.Buffer
	Format(&out, "r.json", testReport(), m)
	for _, want := range []string{"anthropic/claude-sonnet-5", "false-cull rate", "1/2", "missed-cull rate", "2 labeled frames not in the report"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("format lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSweepReviewBelowSharpness(t *testing.T) {
	labels := map[string]string{"A.DNG": "keep", "B.DNG": "keep", "C.DNG": "cull", "D.DNG": "review"}
	rows := Sweep(testReport(), labels, eval.Policy{}, []float64{0, 5.5, 9.5})
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	if rows[0].Matrix.Counts["cull"]["keep"] != 1 || rows[1].Matrix.Counts["cull"]["review"] != 1 || rows[2].Matrix.Counts["keep"]["review"] != 2 {
		t.Fatalf("sweep %+v %+v %+v", rows[0].Matrix.Counts, rows[1].Matrix.Counts, rows[2].Matrix.Counts)
	}
}

func TestSetsAgreement(t *testing.T) {
	g := func(rank int, best bool) *report.Group {
		return &report.Group{ID: 1, Size: 4, Rank: rank, Of: 4, Best: best}
	}
	rep := &report.Report{KeepBest: 2, Results: []report.Result{
		{File: "/s/A.DNG", Group: g(1, true)}, {File: "/s/B.DNG", Group: g(2, true)},
		{File: "/s/C.DNG", Group: g(3, false)}, {File: "/s/D.DNG", Group: g(4, false)},
		{File: "/s/E.DNG"}, // not in a set: ignored
	}}
	labels := map[string]string{"A.DNG": "keep", "B.DNG": "cull", "C.DNG": "keep", "D.DNG": "review", "E.DNG": "keep"}
	s := Sets(rep, labels, 2)
	if s.Kept != 2 || s.KeptRankedOut != 1 || s.Culled != 2 || s.CulledInBest != 1 || s.Sets != 1 {
		t.Fatalf("%+v", s)
	}
	sw := SweepKeepBest(rep, labels, []int{1, 3})
	if sw[0].KeptRankedOut != 1 || sw[1].KeptRankedOut != 0 || sw[1].CulledInBest != 1 {
		t.Fatalf("%+v", sw)
	}
}

// TestSetsRankOnlyKeepBest0 covers a report decided with --keep-best 0 (rank
// only): Group.Best is documented as "rank 1 when KeepBest is 0" (see
// report.Group and pipeline's decideAll, which stores Best with
// max(1, p.KeepBest)), so Sets must treat keepBest 0 the same as keepBest 1.
func TestSetsRankOnlyKeepBest0(t *testing.T) {
	g := func(rank int) *report.Group { return &report.Group{ID: 1, Size: 2, Rank: rank, Of: 2} }
	rep := &report.Report{KeepBest: 0, Results: []report.Result{
		{File: "/s/A.DNG", Group: g(1)}, {File: "/s/B.DNG", Group: g(2)},
	}}
	labels := map[string]string{"A.DNG": "keep", "B.DNG": "keep"}
	s := Sets(rep, labels, rep.KeepBest)
	if s.Kept != 2 || s.KeptRankedOut != 1 {
		t.Fatalf("%+v", s)
	}
}
