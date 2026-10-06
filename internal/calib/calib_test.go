package calib

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
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

func TestJunkFramesCountAndAreListed(t *testing.T) {
	rep := testReport()
	rep.Results = append(rep.Results, report.Result{File: "/shoot/J.DNG", Decision: eval.Cull,
		Junk: &report.JunkInfo{Kind: "white", BrightPct: 99.95}, Reasons: []string{"junk: white"}})
	labels := map[string]string{"A.DNG": "keep", "J.DNG": "keep"}
	m := Compare(rep, labels)
	if m.N != 2 || m.Counts["keep"]["cull"] != 1 {
		t.Fatalf("junk frame not in the matrix: n=%d counts=%v", m.N, m.Counts)
	}
	var out bytes.Buffer
	Format(&out, "r.json", rep, m)
	if !strings.Contains(out.String(), "junk filter: 1 of 1 junk frame(s) you labelled are keeps: J.DNG") {
		t.Fatalf("no junk line:\n%s", out.String())
	}
}

// The grid tries every keep-best × outranked × raw-clipped combination through the
// caller's decide, marks the current one, and stars rows under 1% false culls.
func TestGrid(t *testing.T) {
	labels := map[string]string{"a.DNG": "keep", "b.DNG": "cull"}
	decide := func(p eval.Policy) *report.Report {
		// b is outranked at keep-best 1; a is clipped. Each setting decides one frame.
		b, a := eval.Keep, eval.Keep
		if p.KeepBest < 2 && p.Outranked == eval.ActionCull {
			b = eval.Cull
		}
		if p.RawClipped == eval.ActionReview {
			a = eval.Review
		}
		ev := &eval.Evaluation{}
		return &report.Report{Results: []report.Result{
			{File: "/s/a.DNG", Decision: a, Evaluation: ev},
			{File: "/s/b.DNG", Decision: b, Evaluation: ev},
		}}
	}
	base := eval.Policy{KeepBest: 3, Outranked: eval.ActionReview, RawClipped: eval.ActionReview}
	rows := Grid(labels, base, decide)
	if len(rows) != 20 {
		t.Fatalf("%d rows", len(rows))
	}
	current := 0
	for _, r := range rows {
		if r.Current {
			current++
			if r.KeepBest != 3 || r.Outranked != eval.ActionReview || r.RawClipped != eval.ActionReview {
				t.Fatalf("wrong current row %+v", r)
			}
		}
		caught := r.Matrix.Counts["cull"]["cull"]
		if want := r.KeepBest == 1 && r.Outranked == eval.ActionCull; (caught == 1) != want {
			t.Errorf("row %+v caught %d", r, caught)
		}
	}
	if current != 1 {
		t.Fatalf("%d current rows", current)
	}
	var b strings.Builder
	FormatGrid(&b, rows)
	if !strings.Contains(b.String(), "policy grid") || !strings.Contains(b.String(), "← current") || !strings.Contains(b.String(), "*") {
		t.Fatalf("format:\n%s", b.String())
	}
	t.Log("\n" + b.String())
}
