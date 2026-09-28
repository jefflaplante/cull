package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

// DecideOptions re-apply a policy to a report's stored assessments.
type DecideOptions struct {
	Policy       eval.Policy
	WriteXMP     bool
	XMPDevelop   bool
	OverwriteXMP bool // also overwrite sidecars the report doesn't record as ours
	MoveCulled   bool // sync culled/: move new culls, restore frames no longer culled
	GroupGap     time.Duration
	GroupHamming int
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
	for _, i := range decideAll(rep, o.Policy, group.Options{Gap: o.GroupGap, MaxHamming: o.GroupHamming}) {
		sum.Changed[string(before[i])+"→"+string(rep.Results[i].Decision)]++
	}
	if sum.Frames == 0 {
		return sum, errors.New("no evaluations in report (it came from scan, or every frame failed): run cull first")
	}

	if o.MoveCulled { // restore first, so sidecars are then written where frames live
		for i := range rep.Results {
			r := &rep.Results[i]
			if r.MovedTo == "" || r.Decision == eval.Cull {
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
		sum.Moved = moveCulled(rep, log)
	}
	return sum, rep.Save(reportPath)
}

// writeDecidedSidecar writes the frame's sidecar where the frame currently lives.
// A sidecar the report records as ours is rewritten; any other existing one is
// left alone unless OverwriteXMP.
func writeDecidedSidecar(r *report.Result, o DecideOptions) {
	at := r.File
	if r.MovedTo != "" {
		at = r.MovedTo
	}
	p := xmp.Path(at)
	orientation := 1
	if r.Preview != nil {
		orientation = r.Preview.Orientation
	}
	ours := r.XMP == p
	switch err := xmp.Write(p, buildSidecar(*r, orientation, o.XMPDevelop), ours || o.OverwriteXMP); {
	case err == nil:
		r.XMP = p
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
