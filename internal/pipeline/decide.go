package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

// DecideOptions re-apply a policy to a report's stored assessments.
type DecideOptions struct {
	Policy       eval.Policy
	WriteXMP     bool
	XMPDevelop   bool
	OverwriteXMP bool                    // also overwrite sidecars the report doesn't record as ours
	MoveCulled   bool                    // sync culled/: move new culls, restore frames no longer culled
	Seq          group.Options           // sequences of similar frames; Seq.Gap 0 = no grouping
	Labels       map[string]labels.Entry // the user's labels by base name; nil = the model's verdicts alone
}

// DecideSummary reports what changed.
type DecideSummary struct {
	Frames          int
	Changed         map[string]int // "keep→review" -> count
	Moved, Restored int
}

// Decide re-runs the policy on every stored evaluation without calling a model,
// then optionally rewrites sidecars and syncs culled/. Policy is the only place
// decisions come from, so tuning it after calibration costs nothing.
func Decide(reportPath string, o DecideOptions, log io.Writer) (DecideSummary, error) {
	sum := DecideSummary{Changed: map[string]int{}}
	rep, err := report.Load(reportPath)
	if err != nil {
		return sum, err
	}
	before := make([]eval.Decision, len(rep.Results))
	for i := range rep.Results {
		r := &rep.Results[i]
		before[i] = r.Decision
		if r.Evaluation == nil || r.Error != "" {
			continue
		}
		sum.Frames++
		for _, f := range o.Policy.Sanitize(r.Evaluation) {
			addFixup(r, f)
		}
	}
	for _, i := range decideAll(rep, o.Policy, o.Seq) {
		sum.Changed[string(before[i])+"→"+string(rep.Results[i].Decision)]++
	}
	if sum.Frames == 0 {
		return sum, errors.New("no evaluations in report (it came from scan, or every frame failed): run judge first")
	}
	if dups := labels.Duplicates(rep.Results); len(o.Labels) > 0 && len(dups) > 0 {
		return sum, fmt.Errorf("frames share a file name, so your labels can't tell them apart (rename them, or pass --no-labels): %s", strings.Join(dups, "; "))
	}

	if o.MoveCulled { // restore first, so sidecars are then written where frames live
		for i := range rep.Results {
			r := &rep.Results[i]
			if d, _ := labels.Effective(*r, o.Labels[filepath.Base(r.File)]); r.MovedTo == "" || d == eval.Cull {
				continue
			}
			sidecar, err := relocate(r.MovedTo, r.File)
			if err != nil {
				fmt.Fprintf(log, "not restored %s: %v\n", filepath.Base(r.File), err)
				continue
			}
			followSidecar(r, r.MovedTo, sidecar)
			os.Remove(filepath.Dir(r.MovedTo)) // only succeeds when empty
			r.MovedTo = ""
			sum.Restored++
		}
	}
	if o.WriteXMP {
		for i := range rep.Results {
			r := &rep.Results[i]
			if r.Evaluation == nil || r.Error != "" {
				continue
			}
			writeDecidedSidecar(r, o)
		}
	}
	if o.MoveCulled {
		sum.Moved = moveCulled(rep, o.Labels, log)
	}
	return sum, rep.Save(reportPath)
}

// writeDecidedSidecar writes the frame's sidecar where the frame currently lives,
// from the effective verdict and the user's stars (o.Labels; nil = the model's
// verdict alone). A sidecar the report records as ours is rewritten; any other
// existing one is left alone unless OverwriteXMP.
func writeDecidedSidecar(r *report.Result, o DecideOptions) {
	switch err := labels.WriteSidecar(r, o.Labels[filepath.Base(r.File)], o.XMPDevelop, o.OverwriteXMP); {
	case err == nil:
	case errors.Is(err, xmp.ErrExists):
		addFixup(r, "xmp: sidecar exists and is not ours; not overwritten")
	default:
		addFixup(r, "xmp: "+err.Error())
	}
}

// addFixup records a note once, so repeated decide runs don't pile up copies.
func addFixup(r *report.Result, note string) {
	for _, f := range r.Fixups {
		if f == note {
			return
		}
	}
	r.Fixups = append(r.Fixups, note)
}
