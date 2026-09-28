package pipeline

import (
	"path/filepath"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// decideAll records sequence sets on every measured frame and re-derives every
// evaluated frame's decision from its stored assessment: the policy first, then
// the Duplicates action for frames that aren't the best of their set by scores.
// It returns the indices whose decision changed.
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
	for sid, set := range group.Sequences(frames, o) {
		order := group.ScoreOrder(frames, set)
		bestName := filepath.Base(frames[order[0]].Key)
		for pos, k := range order {
			i := idx[k]
			rep.Results[i].Group = &report.Group{ID: sid + 1, Size: len(set), Rank: pos + 1, Of: len(set), By: "scores", Best: pos == 0}
			if pos > 0 && rep.Results[i].Evaluation != nil {
				decisions[i], reasons[i] = p.ApplyDuplicate(decisions[i], reasons[i], bestName, len(set))
			}
		}
	}

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
