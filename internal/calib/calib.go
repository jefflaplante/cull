// Package calib measures agreement between a report's decisions and hand labels,
// the evidence needed before trusting culls at 1000-frame scale.
package calib

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
)

var classes = []string{"keep", "review", "cull"}

// ReadLabels parses "file,label" rows (header optional, labels keep/review/cull,
// any case). Files are matched by base name; a later row wins.
func ReadLabels(r io.Reader) (map[string]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	labels := map[string]string{}
	for line := 1; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return labels, nil
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 2 || strings.TrimSpace(rec[0]) == "" {
			continue
		}
		file, label := strings.TrimSpace(rec[0]), strings.ToLower(strings.TrimSpace(rec[1]))
		if line == 1 && strings.EqualFold(file, "file") {
			continue
		}
		if label != "keep" && label != "review" && label != "cull" {
			return nil, fmt.Errorf("labels line %d: %q is not keep, review, or cull", line, rec[1])
		}
		labels[filepath.Base(file)] = label
	}
}

// Matrix counts label → decision over labeled, decided frames.
type Matrix struct {
	Counts  map[string]map[string]int
	N       int // labeled frames with a decision
	Missing int // labels with no decided frame in the report
}

func newMatrix() Matrix {
	m := Matrix{Counts: map[string]map[string]int{}}
	for _, c := range classes {
		m.Counts[c] = map[string]int{}
	}
	return m
}

func (m *Matrix) add(label, decision string) {
	m.Counts[label][decision]++
	m.N++
}

// Compare matches the report's decisions against labels.
func Compare(rep *report.Report, labels map[string]string) Matrix {
	return compare(rep, labels, func(r report.Result) string { return string(r.Decision) })
}

func compare(rep *report.Report, labels map[string]string, decide func(report.Result) string) Matrix {
	m := newMatrix()
	matched := map[string]bool{}
	for _, r := range rep.Results {
		name := filepath.Base(r.File)
		l, ok := labels[name]
		if !ok || r.Evaluation == nil || r.Error != "" || r.Decision == "" {
			continue
		}
		matched[name] = true
		m.add(l, decide(r))
	}
	m.Missing = len(labels) - len(matched)
	return m
}

// Rates: false culls (labelled keep, decided cull) over labelled keeps; missed
// culls (labelled cull, decided keep) over labelled culls; share sent to review.
func (m Matrix) Rates() (falseCull, missedCull, review float64) {
	return ratio(m.Counts["keep"]["cull"], rowTotal(m, "keep")),
		ratio(m.Counts["cull"]["keep"], rowTotal(m, "cull")),
		ratio(colTotal(m, "review"), m.N)
}

func rowTotal(m Matrix, label string) int {
	n := 0
	for _, v := range m.Counts[label] {
		n += v
	}
	return n
}

func colTotal(m Matrix, decision string) int {
	n := 0
	for _, row := range m.Counts {
		n += row[decision]
	}
	return n
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// SweepRow is the agreement at one --review-below-sharpness value.
type SweepRow struct {
	Threshold float64
	Matrix    Matrix
}

// Sweep re-decides every labeled frame from its stored assessment with
// ReviewBelowSharpness set to each threshold (bursts are not re-grouped).
func Sweep(rep *report.Report, labels map[string]string, base eval.Policy, thresholds []float64) []SweepRow {
	var rows []SweepRow
	for _, t := range thresholds {
		p := base
		p.ReviewBelowSharpness = t
		m := compare(rep, labels, func(r report.Result) string {
			e := *r.Evaluation
			d, _ := p.DecideFacts(&e, r.Facts())
			return string(d)
		})
		rows = append(rows, SweepRow{Threshold: t, Matrix: m})
	}
	return rows
}

// Format writes the confusion matrix and rates for one report.
func Format(w io.Writer, name string, rep *report.Report, m Matrix) {
	model := rep.Backend + "/" + rep.Model
	if rep.Escalation != "" {
		model += " → " + rep.Escalation
	}
	fmt.Fprintf(w, "report %s (%s): %d labeled frames with decisions\n", name, model, m.N)
	fmt.Fprintf(w, "  %-14s %8s %8s %8s\n", "label \\ decided", "keep", "review", "cull")
	for _, l := range classes {
		fmt.Fprintf(w, "  %-14s %8d %8d %8d\n", l, m.Counts[l]["keep"], m.Counts[l]["review"], m.Counts[l]["cull"])
	}
	fc, mc, rr := m.Rates()
	fmt.Fprintf(w, "  false-cull rate (keep → cull):   %d/%d = %.1f%%\n", m.Counts["keep"]["cull"], rowTotal(m, "keep"), 100*fc)
	fmt.Fprintf(w, "  missed-cull rate (cull → keep):  %d/%d = %.1f%%\n", m.Counts["cull"]["keep"], rowTotal(m, "cull"), 100*mc)
	fmt.Fprintf(w, "  review rate:                     %d/%d = %.1f%%\n", colTotal(m, "review"), m.N, 100*rr)
	if m.Missing > 0 {
		fmt.Fprintf(w, "  %d labeled frames not in the report (or not evaluated)\n", m.Missing)
	}
}

// FormatSweep writes one line per threshold.
func FormatSweep(w io.Writer, rows []SweepRow) {
	fmt.Fprintln(w, "  --review-below-sharpness sweep (re-decided from stored assessments):")
	for _, r := range rows {
		fc, mc, rr := r.Matrix.Rates()
		fmt.Fprintf(w, "    %4.1f  false-cull %5.1f%%  missed-cull %5.1f%%  review %5.1f%%\n", r.Threshold, 100*fc, 100*mc, 100*rr)
	}
}
