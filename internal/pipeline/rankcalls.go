package pipeline

import (
	"context"
	"io"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/report"
)

// RankCalls counts, without spending anything, what `cull rank` would actually do
// to rep right now: the same set selection and the same per-set chunk-plus-final
// call count as Rank/RankSets.
//
// It first fills any looks rep is missing (a schema-v3 report) via fillLooks,
// exactly as Rank does: that mutates rep's Results in place and is free, so a
// caller should persist rep when filled > 0, the same way Rank's own Ctrl-C path
// does. If ctx is cancelled mid-fill, sets and calls come back 0 and err is
// ctx's error; filled still reports how many looks were kept. log receives
// fillLooks's progress (nil-safe: it default to io.Discard) — a large v3 report is
// a full DNG-decode pass here, seconds per frame.
//
// The set selection and counting run on a private copy of rep, decided fresh
// with cfg.Policy and cfg.Seq (decideAll) so the estimate reflects those flags
// even when they differ from the report's last decide. Nothing here writes a
// sidecar, moves a file, or changes rep's own Sets or decisions — only rep's
// Results' Look field, via fillLooks.
func RankCalls(ctx context.Context, rep *report.Report, cfg Config, force bool, log io.Writer) (sets, calls, filled int, err error) {
	filled, err = fillLooks(ctx, rep, log, cfg.UI)
	if err != nil {
		return 0, 0, filled, err
	}
	cp := cloneForRankCalls(rep)
	decideAll(cp, cfg.Policy, cfg.Seq)
	todo := rankTodo(cp, force)
	return len(todo), callsFor(cp, todo, cfg.RankTwice), filled, nil
}

// callsFor is the number of rank calls ranking todo takes: each set's chunks,
// plus a final call merging the chunks' finalists when a set has more than one,
// or, with twice, a reversed-order call when it has exactly one.
func callsFor(rep *report.Report, todo []int, twice bool) int {
	calls := 0
	for _, i := range todo {
		calls += setCalls(rep.Sets[i].Of, twice)
	}
	return calls
}

// rankTodo selects the sets Rank(force) ranks: every set of two or more
// rankable frames with force, otherwise needsRanking's selection (no stored
// model order covering every rankable member). rankSets
// (internal/pipeline/rank.go) uses this same selection, so RankCalls's count
// always matches what a rank run actually does.
func rankTodo(rep *report.Report, force bool) []int {
	if !force {
		return needsRanking(rep)
	}
	var todo []int
	for i, s := range rep.Sets {
		if s.Of >= 2 {
			todo = append(todo, i)
		}
	}
	return todo
}

// cloneForRankCalls copies rep so decideAll can rebuild groups and decisions
// on the copy, for counting, without touching rep's own Sets, Results'
// decisions, or groups.
//
// It is a shallow element copy (each Result and Set is copied by value into a
// fresh slice, but anything a Result or Set points to is shared with rep). That
// is safe only because decideAll reallocates each Result's Group and rebuilds
// Sets from scratch rather than mutating them in place, and Policy only reads
// through *Evaluation, never writing it. Anyone changing decideAll or Policy to
// mutate a Result or Set's pointed-to fields in place must deep-copy here
// instead, or this clone will let counting corrupt rep.
func cloneForRankCalls(rep *report.Report) *report.Report {
	cp := *rep
	cp.Results = append([]report.Result(nil), rep.Results...)
	cp.Sets = append([]report.Set(nil), rep.Sets...)
	return &cp
}

// DecideCopy returns rep decided with p and seq, sets and all, leaving rep's own
// decisions and sets untouched: calibrate's sweep compares against exactly what
// decide would do.
func DecideCopy(rep *report.Report, p eval.Policy, seq group.Options) *report.Report {
	cp := cloneForRankCalls(rep)
	decideAll(cp, p, seq)
	return cp
}

// RankEstimate is the ranking a judge run is expected to do.
type RankEstimate struct {
	Sets, Calls int
	Grouped     bool // from the report's own sets (a scan or an earlier judge); false: the every-frame-in-an-8-frame-set guess
}

// EstimateRanking projects, without spending anything, the rank calls a judge run
// makes once pending is judged. With a report holding measured frames it groups them
// as the run will (cfg.Seq, cfg.Policy) and counts, for each set that will need a
// model order, every member that is rankable now or not judged yet: an upper bound,
// since frames judged cull leave their sets. Pending frames the report doesn't hold
// are priced with the old guess, ⌈n/8⌉ calls. cfg.Rerank counts sets that already
// have a model order; cfg.RankTwice adds the reversed call.
func EstimateRanking(rep *report.Report, pending []string, cfg Config) RankEstimate {
	var e RankEstimate
	unknown := len(pending)
	if rep != nil && measured(rep) {
		e.Grouped = true
		cp := cloneForRankCalls(rep)
		decideAll(cp, cfg.Policy, cfg.Seq)
		held := make(map[string]*report.Result, len(cp.Results))
		for i := range cp.Results {
			held[cp.Results[i].File] = &cp.Results[i]
		}
		for _, s := range cp.Sets {
			unjudged := 0
			for _, f := range s.Members {
				if r := held[f]; r != nil && r.Evaluation == nil {
					unjudged++
				}
			}
			n := s.Of + unjudged
			if n < 2 || (unjudged == 0 && s.By == "model" && !cfg.Rerank) {
				continue
			}
			e.Sets++
			e.Calls += setCalls(n, cfg.RankTwice)
		}
		unknown = 0
		for _, f := range pending {
			if held[f] == nil {
				unknown++
			}
		}
	}
	if unknown > 0 {
		calls := (unknown + maxRankFrames - 1) / maxRankFrames
		if cfg.RankTwice {
			calls *= 2
		}
		e.Calls += calls
	}
	return e
}

func measured(rep *report.Report) bool {
	for _, r := range rep.Results {
		if r.Preview != nil {
			return true
		}
	}
	return false
}

// setCalls is the calls ranking one set of n rankable frames takes: its chunks, plus
// a final merging call when there is more than one, or a reversed call with twice.
func setCalls(n int, twice bool) int {
	parts := len(chunks(n))
	if parts > 1 || twice {
		return parts + 1
	}
	return parts
}
