package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jefflaplante/cull/internal/llm"
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

const locatePrompt = `You locate the intended focus target in photographs from %s. Decide what the photographer meant to be sharp: for people, the eye nearest the camera (a box around both eyes if they are equally near); for animals, the nearest eye; otherwise the most important detail of the main subject. Judge intent from the composition, not from what happens to look sharp. Return a tight box around the target as fractions of the image width and height, measured from the top-left of the image as displayed. If there is no clear subject (an open landscape, an abstract), set confident to false and kind to "none". Keep subject to a few words.`

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

// LocateRequest builds the locate call for a downscaled frame from camera cam.
func LocateRequest(frame []byte, cam Camera, maxTokens int) llm.Request {
	return llm.Request{
		System:     fmt.Sprintf(locatePrompt, cam.Describe()),
		Parts:      []llm.Part{llm.Text("Find the intended focus target in this photograph:"), llm.JPEG(frame)},
		SchemaName: "focus_target",
		Schema:     focusTargetSchema,
		MaxTokens:  maxTokens,
	}
}

// LocateSchema is the locate call's JSON Schema (for validating batch results).
func LocateSchema() map[string]any { return focusTargetSchema }

// DecodeLocate parses a locate answer.
func DecodeLocate(raw []byte) (*LocateResult, error) {
	var loc LocateResult
	if err := json.Unmarshal(raw, &loc); err != nil {
		return nil, fmt.Errorf("decode focus target: %w", err)
	}
	return &loc, nil
}

// Locate asks the backend where the intended focus target is in a downscaled
// frame. Like Evaluate, a result delivered with llm.ErrQuotaStop is returned
// together with that error.
func Locate(ctx context.Context, b llm.Backend, frame []byte, cam Camera, maxTokens int) (*LocateResult, Usage, error) {
	resp, err := b.Call(ctx, LocateRequest(frame, cam, maxTokens))
	if resp == nil || resp.JSON == nil {
		if resp != nil {
			return nil, resp.Usage, err
		}
		return nil, Usage{}, err
	}
	loc, derr := DecodeLocate(resp.JSON)
	if derr != nil {
		return nil, resp.Usage, derr
	}
	return loc, resp.Usage, err
}
