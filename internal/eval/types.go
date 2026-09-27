package eval

import "fmt"

// Evaluation is the model's structured assessment (the forced tool call's input).
type Evaluation struct {
	Sharpness   Sharpness   `json:"sharpness"`
	Exposure    Exposure    `json:"exposure"`
	Composition Composition `json:"composition"`
	Notes       string      `json:"notes"`
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

// Policy encodes: sharpness gates; exposure is fixed, not culled; composition is
// cropped where possible and never culls on its own.
type Policy struct {
	MinCropArea float64
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

// Decide applies the culling policy.
func (p Policy) Decide(e *Evaluation) (Decision, []string) {
	switch e.Sharpness.Status {
	case "missed_focus", "motion_blur":
		return Cull, []string{"sharpness: " + e.Sharpness.Status}
	case "soft":
		return Review, []string{"sharpness: soft"}
	}
	var reasons []string
	d := Keep
	// The preview is tone-mapped; raw usually has more headroom. Until a raw-level
	// clipping check exists, preview clipping routes to review rather than cull.
	if e.Exposure.Status == "clipped" {
		d = Review
		reasons = append(reasons, "exposure: clipped in preview ("+e.Exposure.Clipping+"), verify against raw")
	}
	if e.Composition.Status == "flawed" {
		reasons = append(reasons, "composition: flawed, no crop fix")
	}
	return d, reasons
}
