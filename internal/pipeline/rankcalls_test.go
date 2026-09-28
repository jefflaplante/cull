package pipeline

import (
	"testing"

	"github.com/jefflaplante/gophotocull/internal/report"
)

func TestRankCallsCountsChunkAndFinalCalls(t *testing.T) {
	rep := &report.Report{Sets: []report.Set{
		{ID: 1, Of: 2, By: ""},      // 1 chunk, no final: 1 call
		{ID: 2, Of: 10, By: ""},     // maxRankFrames=8: 2 chunks + 1 final = 3 calls
		{ID: 3, Of: 3, By: "model"}, // already ranked: not counted
		{ID: 4, Of: 1, By: ""},      // not rankable: not counted
	}}
	if got := RankCalls(rep, Config{}); got != 4 {
		t.Fatalf("RankCalls = %d, want 4 (1 + 3)", got)
	}
}

func TestRankCallsOfNothing(t *testing.T) {
	if got := RankCalls(&report.Report{}, Config{}); got != 0 {
		t.Fatalf("RankCalls of an empty report = %d, want 0", got)
	}
}
