package pipeline

import (
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// decideAll re-derives every evaluated frame's decision from its stored
// assessment, groups measured frames into sequence sets, and applies each set's
// order: frames ranked below p.KeepBest that the policy keeps get the Outranked
// action. Ranking only demotes keeps; it never touches review or cull.
//
// A set's order is the model's stored one when that still covers every rankable
// member (members that dropped out are skipped); otherwise it is by the frames'
// own scores. It rebuilds rep.Sets for the current grouping, carrying the model's
// order, notes, summary and cost over, and records p.KeepBest. It returns the
// indices whose decision changed.
func decideAll(rep *report.Report, p eval.Policy, o group.Options) []int {
	var frames []group.Frame
	var idx []int // frames[k] is rep.Results[idx[k]]
	for i := range rep.Results {
		r := &rep.Results[i]
		r.Group = nil
		if r.Error != "" || r.Preview == nil {
			continue
		}
		f := group.Frame{Key: r.File}
		if r.Exif != nil {
			f.Time, f.HasTime = r.Exif.CaptureTime()
		}
		if look, ok := r.LookBytes(); ok {
			f.Look = look
		}
		if e := r.Evaluation; e != nil {
			f.Score = group.Score{Evaluated: true, Sharp: e.Sharpness.Score, EyesOpen: e.People.Eyes == "open",
				Comp: e.Composition.Score, Exp: e.Exposure.Score}
		}
		frames = append(frames, f)
		idx = append(idx, i)
	}

	decisions := make(map[int]eval.Decision)
	reasons := make(map[int][]string)
	for _, i := range idx {
		if e := rep.Results[i].Evaluation; e != nil {
			decisions[i], reasons[i] = p.DecideFacts(e, rep.Results[i].Facts())
		}
	}

	stored := rep.Sets
	storedIn := make(map[string]int) // file -> index into stored, from the model orders
	for si, s := range stored {
		for _, f := range s.Order {
			if _, seen := storedIn[f]; !seen {
				storedIn[f] = si
			}
		}
	}
	carried := make(map[int]bool) // stored sets whose summary and cost a current set took
	best := max(1, p.KeepBest)
	rep.Sets = nil
	for n, set := range group.Sequences(frames, o) {
		s := report.Set{ID: n + 1, By: "scores"}
		member := make(map[string]int, len(set)) // file -> frame index
		var ranked []int                         // rankable frame indices, capture order
		for _, k := range set {
			i := idx[k]
			s.Members = append(s.Members, rep.Results[i].File)
			member[rep.Results[i].File] = k
			if rankable(rep.Results[i], decisions[i]) {
				ranked = append(ranked, k)
			}
		}
		s.Of = len(ranked)

		var order []int // ranked, best first
		if si, ok := modelOrder(stored, storedIn, s.Members, ranked, frames); !ok {
			order = group.ScoreOrder(frames, ranked)
		} else {
			old := stored[si]
			s.By = "model"
			for _, f := range old.Order {
				if k, in := member[f]; in {
					s.Order = append(s.Order, f) // kept whole: a dropped member may come back
					if rankable(rep.Results[idx[k]], decisions[idx[k]]) {
						order = append(order, k)
					}
				}
			}
			for _, nt := range old.Notes {
				if _, in := member[nt.File]; in {
					s.Notes = append(s.Notes, nt)
				}
			}
			if !carried[si] { // a set split by regrouping counts its cost once
				carried[si] = true
				s.Summary, s.Usage, s.CostUSD = old.Summary, old.Usage, old.CostUSD
			}
		}

		rankOf := make(map[int]int, len(order)) // frame index -> 1-based rank
		for pos, k := range order {
			rankOf[k] = pos + 1
		}
		notes := make(map[string]report.RankNote, len(s.Notes))
		for _, nt := range s.Notes {
			notes[nt.File] = nt
		}
		for _, k := range set {
			i := idx[k]
			r := &rep.Results[i]
			g := &report.Group{ID: s.ID, Size: len(set), Rank: rankOf[k], Of: s.Of, By: s.By}
			g.Best = g.Rank >= 1 && g.Rank <= best
			if nt, ok := notes[r.File]; ok && g.Rank > 0 {
				g.Strength, g.Weakness = nt.Strength, nt.Weakness
			}
			r.Group = g
			if p.KeepBest > 0 && g.Rank > p.KeepBest && decisions[i] == eval.Keep {
				decisions[i], reasons[i] = p.ApplyOutranked(decisions[i], reasons[i], g.Rank, g.Of, g.ID, s.By == "scores")
			}
		}
		rep.Sets = append(rep.Sets, s)
	}
	rep.KeepBest = p.KeepBest

	var changed []int
	for _, i := range idx {
		d, ok := decisions[i]
		if !ok {
			continue
		}
		if rep.Results[i].Decision != d {
			changed = append(changed, i)
		}
		rep.Results[i].Decision, rep.Results[i].Reasons = d, reasons[i]
	}
	return changed
}

// modelOrder finds the stored set whose model order holds the current set's
// first rankable member (with none rankable, the first member found in one) and
// reports whether that order still covers every rankable member. A new or newly
// rankable frame the model never saw means it doesn't.
func modelOrder(stored []report.Set, storedIn map[string]int, members []string, ranked []int, frames []group.Frame) (int, bool) {
	if len(ranked) > 0 {
		members = []string{frames[ranked[0]].Key}
	}
	si := -1
	for _, f := range members {
		if s, ok := storedIn[f]; ok {
			si = s
			break
		}
	}
	if si < 0 {
		return 0, false
	}
	in := make(map[string]bool, len(stored[si].Order))
	for _, f := range stored[si].Order {
		in[f] = true
	}
	for _, k := range ranked {
		if !in[frames[k].Key] {
			return 0, false
		}
	}
	return si, true
}

// rankable: evaluated, and the policy doesn't cull it. A technical cull is
// already decided, so it isn't ranked against its set.
func rankable(r report.Result, d eval.Decision) bool {
	return r.Evaluation != nil && d != eval.Cull
}

// needsRanking returns the indices into rep.Sets of the sets that need a model
// ranking: at least two rankable frames and no stored model order covering them.
func needsRanking(rep *report.Report) []int {
	var out []int
	for i, s := range rep.Sets {
		if s.Of >= 2 && s.By != "model" {
			out = append(out, i)
		}
	}
	return out
}
