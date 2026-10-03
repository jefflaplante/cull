// Package calib measures agreement between a report's decisions and hand labels,
// the evidence needed before trusting culls at 1000-frame scale.
package calib

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
)

var classes = []string{"keep", "review", "cull"}

// Matrix counts label → decision over labeled, decided frames.
type Matrix struct {
	Counts  map[string]map[string]int
	N       int // labeled frames with a decision
	Missing int // labels with no decided frame in the report
	// Junk: labelled frames the junk filter flagged; JunkKeeps: those you labelled
	// keep (the filter was wrong about them).
	Junk      int
	JunkKeeps []string
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
		// Junk frames are decided without an assessment, and count like any other.
		if !ok || (r.Evaluation == nil && r.Junk == nil) || r.Error != "" || r.Decision == "" {
			continue
		}
		matched[name] = true
		m.add(l, decide(r))
		if r.Junk != nil {
			m.Junk++
			if l == "keep" {
				m.JunkKeeps = append(m.JunkKeeps, name)
			}
		}
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
// ReviewBelowSharpness set to each threshold. decide, when set, returns the
// whole report decided with a policy (the pipeline's decide: sets regrouped,
// outranked frames demoted), so the sweep matches what decide would do and the
// confusion matrix above it; nil re-decides each frame alone (no set demotion).
func Sweep(rep *report.Report, labels map[string]string, base eval.Policy, thresholds []float64, decide func(eval.Policy) *report.Report) []SweepRow {
	var rows []SweepRow
	for _, t := range thresholds {
		p := base
		p.ReviewBelowSharpness = t
		var m Matrix
		if decide != nil {
			m = Compare(decide(p), labels)
		} else {
			m = compare(rep, labels, func(r report.Result) string {
				if r.Evaluation == nil { // junk: decided without an assessment, no threshold applies
					return string(r.Decision)
				}
				e := *r.Evaluation
				d, _ := p.DecideFacts(&e, r.Facts())
				return string(d)
			})
		}
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
	if m.Junk > 0 {
		fmt.Fprintf(w, "  junk filter: %d of %d junk frame(s) you labelled are keeps", len(m.JunkKeeps), m.Junk)
		if len(m.JunkKeeps) > 0 {
			fmt.Fprintf(w, ": %s", strings.Join(m.JunkKeeps, ", "))
		}
		fmt.Fprintln(w)
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

// SetStats counts how the user's labels line up with each set's stored rank,
// over multi-frame sets (Group.Size > 1) containing at least one labelled frame.
// Kept/Culled are label counts; the *RankedOut/*InBest counts are against
// keepBest (Best = 1 <= Rank <= keepBest), independent of the report's stored
// Group.Best (which may have been computed with a different keep-best).
type SetStats struct {
	Kept          int // labelled "keep" frames in a set
	KeptRankedOut int // of those, rank outside the keep-best cut (or unranked)
	Culled        int // labelled "cull" or "review" frames in a set
	CulledInBest  int // of those, rank inside the keep-best cut
	Sets          int // distinct multi-frame sets with a labelled member
}

// Sets compares labels against each result's stored Group.Rank for the given
// keep-best cut. keepBest 0 (rank-only) is clamped to 1, matching how
// decideAll stores Group.Best (max(1, p.KeepBest)) and how report.Group
// documents Best ("rank 1 when KeepBest is 0"): otherwise a rank-1 keep in a
// rank-only report would be miscounted as ranked out. It does not re-rank;
// SweepKeepBest reuses the same stored ranks.
func Sets(rep *report.Report, labels map[string]string, keepBest int) SetStats {
	if keepBest < 1 {
		keepBest = 1
	}
	var s SetStats
	sets := map[int]bool{}
	for _, r := range rep.Results {
		g := r.Group
		if g == nil || g.Size < 2 {
			continue
		}
		l, ok := labels[filepath.Base(r.File)]
		if !ok {
			continue
		}
		inBest := g.Rank >= 1 && g.Rank <= keepBest
		switch l {
		case "keep":
			s.Kept++
			if !inBest {
				s.KeptRankedOut++
			}
		case "cull", "review":
			s.Culled++
			if inBest {
				s.CulledInBest++
			}
		}
		sets[g.ID] = true
	}
	s.Sets = len(sets)
	return s
}

// KeepBestRow is the set stats recomputed at one keep-best value.
type KeepBestRow struct {
	K int
	SetStats
}

// SweepKeepBest recomputes SetStats at each keep-best value from the stored
// Group.Rank, without re-ranking.
func SweepKeepBest(rep *report.Report, labels map[string]string, ks []int) []KeepBestRow {
	rows := make([]KeepBestRow, len(ks))
	for i, k := range ks {
		rows[i] = KeepBestRow{K: k, SetStats: Sets(rep, labels, k)}
	}
	return rows
}

// FormatSets writes the sets section after the matrix: how often labelled
// keeps got ranked out of the keep-best cut, how often labelled culls or
// reviews made it into that cut, and a keep-best sweep recomputed from the
// stored ranks. It writes nothing when the report has no multi-frame sets
// containing labelled frames.
func FormatSets(w io.Writer, stats SetStats, sweep []KeepBestRow) {
	if stats.Sets == 0 {
		return
	}
	fmt.Fprintf(w, "  sets: %d multi-frame set(s) with labeled frames\n", stats.Sets)
	fmt.Fprintf(w, "    labeled keep, ranked out of best:        %d/%d = %.1f%%\n",
		stats.KeptRankedOut, stats.Kept, 100*ratio(stats.KeptRankedOut, stats.Kept))
	fmt.Fprintf(w, "    labeled cull/review, ranked into best:   %d/%d = %.1f%%\n",
		stats.CulledInBest, stats.Culled, 100*ratio(stats.CulledInBest, stats.Culled))
	if len(sweep) > 0 {
		fmt.Fprintln(w, "  keep-best sweep (recomputed from stored ranks):")
		for _, row := range sweep {
			fmt.Fprintf(w, "    keep-best %d   keep-ranked-out %5.1f%%   cull/review-in-best %5.1f%%\n",
				row.K, 100*ratio(row.KeptRankedOut, row.Kept), 100*ratio(row.CulledInBest, row.Culled))
		}
	}
}
