package pipeline

import (
	"context"

	"github.com/jefflaplante/gophotocull/internal/report"
)

// RankCalls counts, without spending anything, what `cull rank` would actually do
// to rep right now: the same set selection and the same per-set chunk-plus-final
// call count as Rank/RankSets.
//
// It first fills any looks rep is missing (a schema-v3 report) via fillLooks,
// exactly as Rank does: that mutates rep's Results in place and is free, so a
// caller should persist rep when filled > 0, the same way Rank's own Ctrl-C path
// does. If ctx is cancelled mid-fill, sets and calls come back 0 and err is
// ctx's error; filled still reports how many looks were kept.
//
// The set selection and counting run on a private copy of rep, decided fresh
// with cfg.Policy and cfg.Seq (decideAll) so the estimate reflects those flags
// even when they differ from the report's last decide. Nothing here writes a
// sidecar, moves a file, or changes rep's own Sets or decisions — only rep's
// Results' Look field, via fillLooks.
func RankCalls(ctx context.Context, rep *report.Report, cfg Config, force bool) (sets, calls, filled int, err error) {
	filled, err = fillLooks(ctx, rep)
	if err != nil {
		return 0, 0, filled, err
	}
	cp := cloneForRankCalls(rep)
	decideAll(cp, cfg.Policy, cfg.Seq)
	todo := rankTodo(cp, force)
	sets = len(todo)
	for _, i := range todo {
		parts := chunks(cp.Sets[i].Of)
		calls += len(parts)
		if len(parts) > 1 {
			calls++ // the final round, merging the chunks' finalists
		}
	}
	return sets, calls, filled, nil
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
