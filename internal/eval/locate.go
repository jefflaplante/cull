package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jefflaplante/gophotocull/internal/llm"
)

// NormBox is a box in [0,1] fractions of the image as displayed (after EXIF
// orientation), measured from the top-left.
type NormBox struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

// Valid reports whether the box is finite, inside the frame, and non-empty.
func (b NormBox) Valid() bool {
	for _, v := range []float64{b.Left, b.Top, b.Right, b.Bottom} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return false
		}
	}
	return b.Left < b.Right && b.Top < b.Bottom
}

// LocateResult is the model's answer to "what was meant to be sharp?".
type LocateResult struct {
	Confident bool    `json:"confident"`
	Kind      string  `json:"kind"` // eye | face | other | none
	Subject   string  `json:"subject"`
	Box       NormBox `json:"box"`
}

const locatePrompt = `You locate the intended focus target in photographs from a Leica M11-P rangefinder (manual focus, often shot wide open). Decide what the photographer meant to be sharp: for people, the eye nearest the camera (a box around both eyes if they are equally near); for animals, the nearest eye; otherwise the most important detail of the main subject. Judge intent from the composition, not from what happens to look sharp. Return a tight box around the target as fractions of the image width and height, measured from the top-left of the image as displayed. If there is no clear subject (an open landscape, an abstract), set confident to false and kind to "none". Keep subject to a few words.`

var focusTargetSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"confident", "kind", "subject", "box"},
	"properties": map[string]any{
		"confident": map[string]any{"type": "boolean"},
		"kind":      map[string]any{"type": "string", "enum": []string{"eye", "face", "other", "none"}},
		"subject":   map[string]any{"type": "string"},
		"box": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"left", "top", "right", "bottom"},
			"properties": map[string]any{
				"left":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"top":    map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"right":  map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"bottom": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			},
		},
	},
}

// Locate asks the backend where the intended focus target is in a downscaled
// frame. Like Evaluate, a result delivered with llm.ErrQuotaStop is returned
// together with that error.
func Locate(ctx context.Context, b llm.Backend, frame []byte, maxTokens int) (*LocateResult, Usage, error) {
	resp, err := b.Call(ctx, llm.Request{
		System:     locatePrompt,
		Parts:      []llm.Part{llm.Text("Find the intended focus target in this photograph:"), llm.JPEG(frame)},
		SchemaName: "focus_target",
		Schema:     focusTargetSchema,
		MaxTokens:  maxTokens,
	})
	if resp == nil || resp.JSON == nil {
		if resp != nil {
			return nil, resp.Usage, err
		}
		return nil, Usage{}, err
	}
	var loc LocateResult
	if uerr := json.Unmarshal(resp.JSON, &loc); uerr != nil {
		return nil, resp.Usage, fmt.Errorf("decode focus target: %w", uerr)
	}
	return &loc, resp.Usage, err
}
