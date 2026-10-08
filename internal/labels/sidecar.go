package labels

import (
	"path/filepath"
	"time"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

// Effective is the verdict that drives outputs: the user's label if set, else the
// model's decision. yours reports which.
func Effective(r report.Result, l Entry) (d eval.Decision, yours bool) {
	if l.Label != "" {
		return eval.Decision(l.Label), true
	}
	return r.Decision, false
}

// colors are the xmp:Label values Capture One shows as colour tags.
var colors = map[eval.Decision]string{eval.Keep: "Green", eval.Review: "Yellow", eval.Cull: "Red"}

// Sidecar maps a frame and the user's entry for it (zero if none) to sidecar
// metadata: the user's stars as the rating (omitted when unrated; the model never
// sets stars), the effective verdict as colour and keyword, plus
// cull:labeled when the verdict is the user's and cull:best when it is the top
// of its set (Group.Best). Develop settings only when asked, and never for a cull.
func Sidecar(r report.Result, l Entry, _ int, develop bool, tags *report.Tags) xmp.Sidecar {
	d, yours := Effective(r, l)
	sc := xmp.Sidecar{Rating: l.Stars, Label: colors[d]}
	if t, err := time.Parse("2006-01-02T15:04:05", r.DatesSet); err == nil {
		sc.DateTaken = t.Format("2006-01-02T15:04:05")
	} else if r.Exif != nil {
		if t, err := time.Parse("2006:01:02 15:04:05", r.Exif.DateTimeOriginal); err == nil {
			sc.DateTaken = t.Format("2006-01-02T15:04:05")
		}
	}
	if d != "" {
		sc.Keywords = append(sc.Keywords, "cull:"+string(d))
	}
	if yours {
		sc.Keywords = append(sc.Keywords, "cull:labeled")
	}
	if r.Group != nil && r.Group.Best {
		sc.Keywords = append(sc.Keywords, "cull:best")
	}
	// Content keywords (the model's; junk frames have none) and the shoot's tags: plain
	// words in dc:subject, and paths that Capture One and Lightroom nest.
	if r.Evaluation != nil {
		for _, k := range r.Evaluation.Keywords {
			sc.Keywords = append(sc.Keywords, k)
			sc.Hierarchy = append(sc.Hierarchy, "content|"+k)
		}
	}
	sc.Keywords = dedupe(append(sc.Keywords, tags.Plain()...))
	sc.Hierarchy = dedupe(append(sc.Hierarchy, tags.Paths()...))
	if l.EV != nil { // yours, from review: for Adobe tools (Capture One ignores it; apply-c1 carries it there)
		v := *l.EV
		sc.ExposureEV = &v
	}
	if !develop || r.Evaluation == nil || d == eval.Cull {
		return sc
	}
	e := r.Evaluation
	if e.Exposure.Status == "fixable" && sc.ExposureEV == nil {
		v := e.Exposure.EVAdjust
		sc.ExposureEV = &v
	}
	if c := e.Composition.Crop; c.Apply {
		// crs:Crop* are oriented-frame edges (see xmp.DisplayCrop); the model's
		// crop is already in display coordinates, so it passes through as-is.
		b := xmp.DisplayCrop(c.Left, c.Top, c.Right, c.Bottom)
		sc.Crop = &b
	}
	return sc
}

// WriteSidecar writes the frame's sidecar where the frame now lives (its culled/
// path if moved). A sidecar the report records as ours, or that still carries
// cull's marker (xmp.Ours: its record was lost in a crash), is rewritten; any other
// existing one is left alone (xmp.ErrExists) unless overwrite. On success the path
// is recorded in r.XMP, and whether it carries develop settings in r.XMPDevelop.
func WriteSidecar(r *report.Result, l Entry, develop, overwrite bool, tags *report.Tags) error {
	at := r.File
	if r.MovedTo != "" {
		at = r.MovedTo
	}
	p := xmp.Path(at)
	orientation := 1
	if r.Preview != nil {
		orientation = r.Preview.Orientation
	}
	if err := xmp.Write(p, Sidecar(*r, l, orientation, develop, tags), r.XMP == p || overwrite || xmp.Ours(p)); err != nil {
		return err
	}
	r.XMP, r.XMPDevelop = p, develop
	return nil
}

// Duplicates lists frames that share a base name ("a and b"): labels are keyed by
// base name, so they can't say which of those frames they mean.
func Duplicates(results []report.Result) []string {
	seen := map[string]string{}
	var dups []string
	for _, r := range results {
		b := filepath.Base(r.File)
		if prev, ok := seen[b]; ok {
			dups = append(dups, prev+" and "+r.File)
			continue
		}
		seen[b] = r.File
	}
	return dups
}

// dedupe keeps each keyword's first occurrence, in order: a tag can repeat one of the
// model's content keywords.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, k := range in {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
