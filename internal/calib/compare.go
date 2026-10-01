package calib

import (
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jefflaplante/cull/internal/report"
)

// RunDiff is how two reports over the same frames disagree: two runs of one
// backend (run-to-run stability) or two backends.
type RunDiff struct {
	N                 int            // frames assessed and decided in both
	Same              int            // of those, with the same decision
	Flips             map[string]int // "keep→cull" -> count, for differing decisions
	StatusFlips       int            // frames whose sharpness status differs
	MeanAbsSharpDelta float64        // mean |sharpness score a - b|
	Missing           int            // frames assessed in only one of the two
}

// CompareRuns matches frames by base name, as labels are, so two runs written to
// different report paths (or over a moved folder) still line up.
// It refuses when frames share a base name in either report (a recursive shoot):
// they couldn't be told apart.
func CompareRuns(a, b *report.Report) (RunDiff, error) {
	d := RunDiff{Flips: map[string]int{}}
	var dups []string
	assessed := func(rep *report.Report) map[string]report.Result {
		m := map[string]report.Result{}
		for _, r := range rep.Results {
			if r.Evaluation != nil && r.Error == "" && r.Decision != "" {
				name := filepath.Base(r.File)
				if prev, ok := m[name]; ok {
					dups = append(dups, prev.File+" and "+r.File)
				}
				m[name] = r
			}
		}
		return m
	}
	am, bm := assessed(a), assessed(b)
	if len(dups) > 0 {
		return d, fmt.Errorf("frames share a file name, so the two runs can't be matched frame by frame: %s", strings.Join(dups, "; "))
	}
	var sum float64
	for name, ra := range am {
		rb, ok := bm[name]
		if !ok {
			d.Missing++
			continue
		}
		d.N++
		if ra.Decision == rb.Decision {
			d.Same++
		} else {
			d.Flips[string(ra.Decision)+"→"+string(rb.Decision)]++
		}
		if ra.Evaluation.Sharpness.Status != rb.Evaluation.Sharpness.Status {
			d.StatusFlips++
		}
		sum += math.Abs(ra.Evaluation.Sharpness.Score - rb.Evaluation.Sharpness.Score)
	}
	for name := range bm {
		if _, ok := am[name]; !ok {
			d.Missing++
		}
	}
	if d.N > 0 {
		d.MeanAbsSharpDelta = sum / float64(d.N)
	}
	return d, nil
}

// FormatRunDiff writes the comparison. Crossings (keep in one run, cull in the
// other) are called out: they are the disagreements that cost a photo.
func FormatRunDiff(w io.Writer, an, bn string, d RunDiff) {
	fmt.Fprintf(w, "%s vs %s: %d frames in both\n", an, bn, d.N)
	fmt.Fprintf(w, "  decisions agree on %d/%d = %.1f%%\n", d.Same, d.N, 100*ratio(d.Same, d.N))
	var kinds []string
	for k := range d.Flips {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(w, "    %s %d\n", k, d.Flips[k])
	}
	fmt.Fprintf(w, "  keep↔cull crossings: %d\n", d.Flips["keep→cull"]+d.Flips["cull→keep"])
	fmt.Fprintf(w, "  sharpness status differs on %d/%d\n", d.StatusFlips, d.N)
	fmt.Fprintf(w, "  mean |Δ sharpness score|: %.2f\n", d.MeanAbsSharpDelta)
	if d.Missing > 0 {
		fmt.Fprintf(w, "  %d frame(s) assessed in only one report\n", d.Missing)
	}
}
