package pipeline

import "github.com/jefflaplante/gophotocull/internal/report"

// RankCalls counts the model calls that ranking rep's sets needing it (Task 10:
// cull rank --estimate) would make: each set's chunk calls (chunks(n) for its
// rankable member count, Of, as of the last decide) plus one final call when it
// was chunked into more than one. It costs nothing to compute: no image is
// loaded and no model is called, and it doesn't depend on cfg beyond identifying
// the report (kept for symmetry with the rest of the rank stage, and so a future
// cfg-dependent policy on call sizing has somewhere to live).
func RankCalls(rep *report.Report, cfg Config) int {
	_ = cfg
	n := 0
	for _, i := range needsRanking(rep) {
		parts := chunks(rep.Sets[i].Of)
		n += len(parts)
		if len(parts) > 1 {
			n++ // the final round, merging the chunks' finalists
		}
	}
	return n
}
