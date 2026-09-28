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
