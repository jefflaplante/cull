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
// Grid re-decides from the same stored ranks.
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

// FormatSets writes the sets section after the matrix: how often labelled
// keeps got ranked out of the keep-best cut, how often labelled culls or
// reviews made it into that cut. It writes nothing when the report has no multi-frame sets
// containing labelled frames.
func FormatSets(w io.Writer, stats SetStats) {
	if stats.Sets == 0 {
		return
	}
	fmt.Fprintf(w, "  sets: %d multi-frame set(s) with labeled frames\n", stats.Sets)
	fmt.Fprintf(w, "    labeled keep, ranked out of best:        %d/%d = %.1f%%\n",
		stats.KeptRankedOut, stats.Kept, 100*ratio(stats.KeptRankedOut, stats.Kept))
	fmt.Fprintf(w, "    labeled cull/review, ranked into best:   %d/%d = %.1f%%\n",
		stats.CulledInBest, stats.Culled, 100*ratio(stats.CulledInBest, stats.Culled))
}

// GridRow is the agreement under one combination of the three settings that moved
// the first calibration most (2026-10-05): keep-best, outranked and raw-clipped.
type GridRow struct {
	KeepBest              int
	Outranked, RawClipped eval.Action
	Matrix                Matrix
	Current               bool // the policy the report's decisions came from (or the flags given)
}

// Grid re-decides the report under every keep-best 1-5 × outranked review/cull ×
// raw-clipped review/ignore combination, the rest of the policy as base. decide is
// the pipeline's (sets regrouped, stored ranks applied), so each row is what
// 'cull decide' with those flags would give.
func Grid(labels map[string]string, base eval.Policy, decide func(eval.Policy) *report.Report) []GridRow {
	var rows []GridRow
	for k := 1; k <= 5; k++ {
		for _, o := range []eval.Action{eval.ActionReview, eval.ActionCull} {
			for _, rc := range []eval.Action{eval.ActionReview, eval.ActionIgnore} {
				p := base
				p.KeepBest, p.Outranked, p.RawClipped = k, o, rc
				rows = append(rows, GridRow{KeepBest: k, Outranked: o, RawClipped: rc, Matrix: Compare(decide(p), labels),
					Current: k == base.KeepBest && o == base.Outranked && rc == base.RawClipped})
			}
		}
	}
	return rows
}

// FormatGrid writes one line per combination: false culls (you said keep, it
// culled), culls caught, missed culls (you said cull, it kept) and the review rate.
func FormatGrid(w io.Writer, rows []GridRow) {
	fmt.Fprintln(w, "  policy grid (re-decided from stored assessments and ranks; * = false culls under 1%):")
	fmt.Fprintf(w, "      %-9s %-9s %-11s %14s %7s %7s %7s\n", "keep-best", "outranked", "raw-clipped", "false culls", "caught", "missed", "review")
	for _, r := range rows {
		m := r.Matrix
		fc, _, rr := m.Rates()
		mark, cur := " ", ""
		if fc < 0.01 {
			mark = "*"
		}
		if r.Current {
			cur = "  ← current"
		}
		fmt.Fprintf(w, "    %s %-9d %-9s %-11s %6d (%4.1f%%) %7d %7d %6.1f%%%s\n", mark, r.KeepBest, r.Outranked, r.RawClipped,
			m.Counts["keep"]["cull"], 100*fc, m.Counts["cull"]["cull"], m.Counts["cull"]["keep"], 100*rr, cur)
	}
}
