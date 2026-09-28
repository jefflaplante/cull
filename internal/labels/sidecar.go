package labels

import (
	"path/filepath"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
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
// cull:labeled when the verdict is the user's. Develop settings only when
// asked, and never for a cull.
func Sidecar(r report.Result, l Entry, orientation int, develop bool) xmp.Sidecar {
	d, yours := Effective(r, l)
	sc := xmp.Sidecar{Rating: l.Stars, Label: colors[d]}
	if d != "" {
		sc.Keywords = append(sc.Keywords, "cull:"+string(d))
	}
	if yours {
		sc.Keywords = append(sc.Keywords, "cull:labeled")
	}
	if !develop || r.Evaluation == nil || d == eval.Cull {
		return sc
	}
	e := r.Evaluation
	if e.Exposure.Status == "fixable" {
		v := e.Exposure.EVAdjust
		sc.ExposureEV = &v
	}
	if c := e.Composition.Crop; c.Apply {
		b := xmp.FromDisplay(c.Left, c.Top, c.Right, c.Bottom, orientation)
		sc.Crop = &b
	}
	return sc
}

// WriteSidecar writes the frame's sidecar where the frame now lives (its culled/
// path if moved). A sidecar the report records as ours is rewritten; any other
// existing one is left alone (xmp.ErrExists) unless overwrite. On success the path
// is recorded in r.XMP.
func WriteSidecar(r *report.Result, l Entry, develop, overwrite bool) error {
	at := r.File
	if r.MovedTo != "" {
		at = r.MovedTo
	}
	p := xmp.Path(at)
	orientation := 1
	if r.Preview != nil {
		orientation = r.Preview.Orientation
	}
	if err := xmp.Write(p, Sidecar(*r, l, orientation, develop), r.XMP == p || overwrite); err != nil {
		return err
	}
	r.XMP = p
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
