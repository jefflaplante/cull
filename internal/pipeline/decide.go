package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
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
// decisions come from, so tuning it after calibration costs nothing. A schema-v3
// report gets its looks computed from the DNGs and is saved as the current
// schema. ctx bounds only that look computation: mirroring pipeline.Rank, a
// cancelled ctx (Ctrl-C) saves the looks computed so far (if any) and returns the
// context's error before redecide runs, so no sidecar is rewritten and no file
// is moved.
func Decide(ctx context.Context, reportPath string, o DecideOptions, log io.Writer) (DecideSummary, error) {
	rep, err := report.Load(reportPath)
	if err != nil {
		return DecideSummary{Changed: map[string]int{}}, err
	}
	n, err := fillLooks(ctx, rep, log)
	if n > 0 {
		fmt.Fprintf(log, "computed the look of %d frame(s) from their DNGs\n", n)
	}
	if err != nil { // Ctrl-C: keep the looks computed so far (free but slow); decide and move nothing
		if n > 0 {
			err = errors.Join(err, rep.Save(reportPath))
		}
		return DecideSummary{Changed: map[string]int{}}, err
	}
	sum, err := redecide(rep, o, log, nil)
	if err != nil {
		return sum, err
	}
	return sum, rep.Save(reportPath)
}

// redecide re-applies the policy to rep's stored assessments. between, when set,
// runs after the first decide (ranking) and is followed by a second. Then it
// restores frames no longer culled, writes sidecars and moves culls from the final
// decisions, and marks rep as the current schema; the caller saves. When there is
// nothing to decide or labels can't be matched it returns an error before between
// and before touching any file.
func redecide(rep *report.Report, o DecideOptions, log io.Writer, between func()) (DecideSummary, error) {
	sum := DecideSummary{Changed: map[string]int{}}
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
	decideAll(rep, o.Policy, o.Seq)
	if sum.Frames == 0 {
		return sum, errors.New("no evaluations in report (it came from scan, or every frame failed): run judge first")
	}
	if dups := labels.Duplicates(rep.Results); len(o.Labels) > 0 && len(dups) > 0 {
		return sum, fmt.Errorf("frames share a file name, so your labels can't tell them apart (rename them, or pass --no-labels): %s", strings.Join(dups, "; "))
	}
	if between != nil {
		between()
		decideAll(rep, o.Policy, o.Seq)
	}
	for i := range rep.Results {
		if d := rep.Results[i].Decision; d != before[i] {
			sum.Changed[string(before[i])+"→"+string(d)]++
		}
	}
	rep.SchemaVersion = report.SchemaVersion
	for i := range rep.Results {
		reconcileMove(&rep.Results[i]) // files moved or restored by a run whose report was never saved
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
	return sum, nil
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
