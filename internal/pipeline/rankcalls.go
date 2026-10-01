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
	filled, err = fillLooks(ctx, rep, log)
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
		parts := chunks(rep.Sets[i].Of)
		calls += len(parts)
		if len(parts) > 1 || twice {
			calls++
		}
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
