package pipeline

import (
	"fmt"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/report"
)

// decideAll re-derives every evaluated frame's decision from its stored
// assessment, groups measured frames into sequence sets, and applies each set's
// order: frames ranked below p.KeepBest that the policy keeps get the Outranked
// action. Ranking only demotes keeps; it never touches review or cull.
//
// A set's order is the model's stored one (By "model") when that still covers
// every rankable member; members that dropped out are skipped. Otherwise, as when
// a newly rankable frame was never compared, it is by the frames' own scores (By
// "scores") and the set needs ranking. It rebuilds rep.Sets for the current
// grouping and records p.KeepBest. A stored set matched through its members
// passes its order, notes, summary and cost to the new set whether or not the
// order still covers it, so a policy change that is undone restores the model's
// ranking. A stored order whose set no longer exists after a regrouping (another
// gap or look, a split set merged again) is dropped. It returns the indices whose
// decision changed.
func decideAll(rep *report.Report, p eval.Policy, o group.Options) []int {
	var frames []group.Frame
	var idx []int // frames[k] is rep.Results[idx[k]]
	for i := range rep.Results {
		r := &rep.Results[i]
		r.Group = nil
		if r.Error != "" || r.Preview == nil || junked(*r, p) {
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

		// A matched stored set's paid order is carried even when it no longer covers
		// the rankable members, so a later decide that restores coverage uses it.
		matched := false
		if si, ok := storedFor(storedIn, ranked, s.Members, frames); ok {
			matched = true
			old := stored[si]
			for _, f := range old.Order {
				if _, in := member[f]; in {
					s.Order = append(s.Order, f) // kept whole: a dropped member may come back
				}
			}
			for _, f := range old.Reversed {
				if _, in := member[f]; in {
					s.Reversed = append(s.Reversed, f)
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
		var order []int // rankable frames, best first
		unseen := make(map[int]bool, len(ranked))
		for _, k := range ranked {
			unseen[k] = true
		}
		for _, f := range s.Order {
			if k := member[f]; unseen[k] {
				order = append(order, k)
				delete(unseen, k)
			}
		}
		if matched && len(unseen) == 0 {
			s.By = "model"
		} else { // a frame the model never compared: by scores until ranked again
			order = group.ScoreOrder(frames, ranked)
		}

		rankOf := make(map[int]int, len(order)) // frame index -> 1-based rank
		for pos, k := range order {
			rankOf[k] = pos + 1
		}
		// The reversed order counts only while it, too, covers every rankable member.
		rank2Of := map[int]int{}
		if s.By == "model" && len(s.Reversed) > 0 {
			for _, f := range s.Reversed {
				if k, in := member[f]; in && rankable(rep.Results[idx[k]], decisions[idx[k]]) {
					rank2Of[k] = len(rank2Of) + 1
				}
			}
			if len(rank2Of) != len(ranked) {
				rank2Of = map[int]int{}
			}
		}
		notes := make(map[string]report.RankNote, len(s.Notes))
		for _, nt := range s.Notes {
			notes[nt.File] = nt
		}
		for _, k := range set {
			i := idx[k]
			r := &rep.Results[i]
			g := &report.Group{ID: s.ID, Size: len(set), Rank: rankOf[k], Of: s.Of, By: s.By}
			rank2 := rank2Of[k] // 0 = ranked once
			g.Best = g.Rank >= 1 && g.Rank <= best && (rank2 == 0 || rank2 <= best)
			if nt, ok := notes[r.File]; ok && g.Rank > 0 && s.By == "model" {
				g.Strength, g.Weakness = nt.Strength, nt.Weakness
			}
			r.Group = g
			disputed := rank2 > 0 && g.Rank > 0 && (g.Rank <= p.KeepBest) != (rank2 <= p.KeepBest)
			switch {
			case p.KeepBest == 0 || decisions[i] != eval.Keep:
			case disputed:
				decisions[i], reasons[i] = p.ApplyDisputed(decisions[i], reasons[i], g.Rank, rank2, g.Of, g.ID)
			case g.Rank > p.KeepBest:
				decisions[i], reasons[i] = p.ApplyOutranked(decisions[i], reasons[i], g.Rank, g.Of, g.ID, s.By == "scores")
			}
		}
		rep.Sets = append(rep.Sets, s)
	}
	rep.KeepBest = p.KeepBest
	rep.Policy = &p
	rep.Seq = report.SequencesOf(o)

	var changed []int
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.Error != "" || r.Junk == nil {
			continue
		}
		d, rs, ok := p.DecideJunk(JunkDetail(r.Junk))
		if !ok && r.Evaluation != nil {
			continue // judged by the model (--junk ignore): decided with the rest below
		}
		if !ok { // --junk ignore, never judged: no verdict until a judge run sends it
			d, rs = "", []string{"junk: " + JunkDetail(r.Junk) + "; --junk ignore: run judge again to have it judged"}
		}
		if r.Decision != d {
			changed = append(changed, i)
		}
		r.Decision, r.Reasons = d, rs
	}
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

// storedFor finds the stored set whose model order holds the current set's first
// rankable member found in one or, failing that, any member.
func storedFor(storedIn map[string]int, ranked []int, members []string, frames []group.Frame) (int, bool) {
	for _, k := range ranked {
		if si, ok := storedIn[frames[k].Key]; ok {
			return si, true
		}
	}
	for _, f := range members {
		if si, ok := storedIn[f]; ok {
			return si, true
		}
	}
	return 0, false
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

// junked reports whether r is decided as junk under p: flagged, and the policy
// doesn't say to judge junk anyway. A frame judged before (--junk ignore then) keeps
// its assessment out of it.
func junked(r report.Result, p eval.Policy) bool {
	if r.Junk == nil {
		return false
	}
	_, _, ok := p.DecideJunk("")
	return ok
}

// JunkDetail describes why a frame is junk, with the number that decided it.
func JunkDetail(j *report.JunkInfo) string {
	switch j.Kind {
	case "black":
		return fmt.Sprintf("black (%.2f%% of pixels near-black)", j.DarkPct)
	case "white":
		return fmt.Sprintf("white (%.2f%% of pixels blown)", j.BrightPct)
	}
	return fmt.Sprintf("%s (thumbnail contrast %.2f)", j.Kind, j.Contrast)
}
