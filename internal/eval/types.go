package eval

import "fmt"

// Evaluation is the model's structured assessment (the forced tool call's input).
type Evaluation struct {
	Sharpness   Sharpness   `json:"sharpness"`
	Exposure    Exposure    `json:"exposure"`
	Composition Composition `json:"composition"`
	People      People      `json:"people"`
	Notes       string      `json:"notes"`
}

// People flags things sharpness can't see: blinks and unflattering moments.
type People struct {
	Present    bool   `json:"present"`
	Eyes       string `json:"eyes"`       // open|closed|partial|not_visible
	Expression string `json:"expression"` // good|neutral|awkward|not_applicable
}

type Sharpness struct {
	Score       float64 `json:"score"`
	Status      string  `json:"status"` // sharp|acceptable|soft|missed_focus|motion_blur
	FocusTarget string  `json:"focus_target"`
}

type Exposure struct {
	Score    float64 `json:"score"`
	Status   string  `json:"status"` // good|fixable|clipped
	EVAdjust float64 `json:"ev_adjust"`
	Clipping string  `json:"clipping"` // none|highlights|shadows|both
	Reason   string  `json:"reason"`
}

type Composition struct {
	Score             float64  `json:"score"`
	Status            string   `json:"status"` // good|croppable|flawed
	Issues            []string `json:"issues"`
	Crop              Crop     `json:"crop"`
	StraightenDegrees float64  `json:"straighten_degrees"` // positive = rotate clockwise
}

// Crop edges are normalized [0,1] from the top-left of the image AS DISPLAYED
// (after EXIF orientation).
type Crop struct {
	Apply  bool    `json:"apply"`
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

func (c Crop) Area() float64 { return (c.Right - c.Left) * (c.Bottom - c.Top) }

// Decision is computed by Policy, not taken from the model, so culling behaviour
// is deterministic and auditable given the model's assessments.
type Decision string

const (
	Keep   Decision = "keep"
	Review Decision = "review"
	Cull   Decision = "cull"
)

// Action is what the policy does with a signal: nothing, send to review, or cull.
// The zero value means review, the conservative default for every new signal.
type Action string

const (
	ActionIgnore Action = "ignore"
	ActionReview Action = "review"
	ActionCull   Action = "cull"
)

// ParseAction validates a flag value.
func ParseAction(s string) (Action, error) {
	switch a := Action(s); a {
	case ActionIgnore, ActionReview, ActionCull:
		return a, nil
	}
	return "", fmt.Errorf("%q: want ignore, review, or cull", s)
}

func (a Action) decision() (Decision, bool) {
	switch a {
	case ActionIgnore:
		return Keep, false
	case ActionCull:
		return Cull, true
	}
	return Review, true
}

// Policy encodes: sharpness gates; exposure is fixed, not culled; composition is
// cropped where possible and never culls on its own. Other signals default to
// review and are culled only when configured to.
type Policy struct {
	MinCropArea          float64
	ReviewBelowSharpness float64 // 0 = off; keep -> review when the sharpness score is lower
	EyesClosed           Action
	RawClipped           Action  // raw highlights clipped beyond RawClipThreshold
	RawClipThreshold     float64 // percent of raw samples at white level; 0 = default 0.5
	KeepBest             int     // per set, keep this many best-ranked frames; 0 = rank only
	Outranked            Action  // frames ranked below KeepBest in their set
}

// Facts are measurements the policy uses beside the model's assessment.
type Facts struct {
	RawKnown   bool    // raw clipping was measured
	RawClipPct float64 // percent of raw samples at the white level
}

// ApplyOutranked raises a frame ranked below KeepBest in its set by the Outranked
// action (review by default) and always leaves the reason.
func (p Policy) ApplyOutranked(d Decision, reasons []string, pos, of, set int, byScores bool) (Decision, []string) {
	if to, act := p.Outranked.decision(); act && rank(to) > rank(d) {
		d = to
	}
	how := ""
	if byScores {
		how = " by scores, not compared"
	}
	return d, append(reasons, fmt.Sprintf("rank %d of %d in set %d%s (keeping the best %d)", pos, of, set, how, p.KeepBest))
}

// Sanitize clamps values and drops invalid crops. Returns human-readable fixups.
func (p Policy) Sanitize(e *Evaluation) []string {
	var notes []string
	clamp := func(v, lo, hi float64) float64 {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	e.Sharpness.Score = clamp(e.Sharpness.Score, 0, 10)
	e.Exposure.Score = clamp(e.Exposure.Score, 0, 10)
	e.Composition.Score = clamp(e.Composition.Score, 0, 10)
	e.Exposure.EVAdjust = clamp(e.Exposure.EVAdjust, -4, 4)

	c := &e.Composition.Crop
	if c.Apply {
		valid := c.Left >= 0 && c.Top >= 0 && c.Right <= 1 && c.Bottom <= 1 && c.Left < c.Right && c.Top < c.Bottom
		switch {
		case !valid:
			notes = append(notes, fmt.Sprintf("dropped invalid crop %+v", *c))
			*c = Crop{}
		case c.Area() < p.MinCropArea:
			notes = append(notes, fmt.Sprintf("dropped crop retaining %.0f%% (< %.0f%% minimum)", 100*c.Area(), 100*p.MinCropArea))
			*c = Crop{}
		}
	}
	return notes
}

// Decide applies the policy to an assessment alone.
func (p Policy) Decide(e *Evaluation) (Decision, []string) { return p.DecideFacts(e, Facts{}) }

// DecideFacts applies the culling policy: the most severe outcome of any rule
// wins (cull > review > keep), and every rule that fired leaves a reason.
func (p Policy) DecideFacts(e *Evaluation, f Facts) (Decision, []string) {
	d := Keep
	var reasons []string
	raise := func(to Decision, reason string) {
		if rank(to) > rank(d) {
			d = to
		}
		reasons = append(reasons, reason)
	}
	note := func(reason string) { reasons = append(reasons, reason) }

	switch e.Sharpness.Status {
	case "missed_focus", "motion_blur":
		raise(Cull, "sharpness: "+e.Sharpness.Status)
	case "soft":
		raise(Review, "sharpness: soft")
	default:
		if p.ReviewBelowSharpness > 0 && e.Sharpness.Score < p.ReviewBelowSharpness {
			raise(Review, fmt.Sprintf("sharpness %.1f below %.1f", e.Sharpness.Score, p.ReviewBelowSharpness))
		}
	}
	// The preview is tone-mapped and overstates clipping; the raw decides when it
	// was measured. Without it, preview clipping routes to review, never cull.
	threshold := p.RawClipThreshold
	if threshold <= 0 {
		threshold = 0.5
	}
	switch {
	case f.RawKnown && f.RawClipPct >= threshold:
		if to, act := p.RawClipped.decision(); act {
			raise(to, fmt.Sprintf("raw highlights clipped: %.2f%% of samples at white level", f.RawClipPct))
		}
	case f.RawKnown && e.Exposure.Status == "clipped":
		note(fmt.Sprintf("exposure: preview looks clipped but the raw retains highlights (%.3f%% at white level)", f.RawClipPct))
	case e.Exposure.Status == "clipped":
		raise(Review, "exposure: clipped in preview ("+e.Exposure.Clipping+"), verify against raw")
	}
	if e.Composition.Status == "flawed" {
		note("composition: flawed, no crop fix")
	}
	switch e.People.Eyes {
	case "closed":
		if to, act := p.EyesClosed.decision(); act {
			raise(to, "eyes closed")
		}
	case "partial":
		note("eyes partially closed")
	}
	if e.People.Expression == "awkward" {
		note("expression: awkward")
	}
	return d, reasons
}

func rank(d Decision) int {
	switch d {
	case Cull:
		return 2
	case Review:
		return 1
	}
	return 0
}
