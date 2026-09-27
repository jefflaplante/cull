package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jefflaplante/gophotocull/internal/report"
)

// ScanSummary condenses a scan into one line: whether previews are big enough
// to judge focus, where they came from, orientation mix, and face coverage.
func ScanSummary(rep *report.Report) string {
	var edges []int
	sources, orients := map[string]int{}, map[int]int{}
	faces, targets := 0, 0
	for _, r := range rep.Results {
		if pv := r.Preview; pv != nil {
			edges = append(edges, max(pv.Width, pv.Height))
			sources[pv.Source]++
			orients[pv.Orientation]++
		}
		if ft := r.FocusTarget; ft != nil {
			targets++
			if ft.Source == "face" {
				faces++
			}
		}
	}
	if len(edges) == 0 {
		return "previews: none"
	}
	sort.Ints(edges)
	var src []string
	for k, n := range sources {
		src = append(src, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(src)
	var ori []int
	for k := range orients {
		ori = append(ori, k)
	}
	sort.Ints(ori)
	var ors []string
	for _, k := range ori {
		ors = append(ors, fmt.Sprintf("%d=%d", k, orients[k]))
	}
	return fmt.Sprintf("previews: long edge min/median/max = %d/%d/%d px; sources: %s; orientation: %s; faces: %d/%d",
		edges[0], edges[len(edges)/2], edges[len(edges)-1], strings.Join(src, " "), strings.Join(ors, " "), faces, targets)
}
