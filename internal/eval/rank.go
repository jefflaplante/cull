package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jefflaplante/gophotocull/internal/llm"
)

// RankFrame is one frame's images for a rank call: the full frame and,
// where a subject was located, its native-resolution crop. Crop may be nil.
type RankFrame struct {
	Full, Crop []byte
}

// RankEntry is one frame's place in a ranking, best first.
type RankEntry struct {
	Frame    int    `json:"frame"`
	Strength string `json:"strength"`
	Weakness string `json:"weakness"`
}

// Ranking is the model's answer to a rank call: every input frame exactly
// once, best first.
type Ranking struct {
	Ranking []RankEntry `json:"ranking"`
	Summary string      `json:"summary"`
}

const rankPrompt = `You compare a small set of near-duplicate photographs from the same moment and rank them best to worst. The frames are near-duplicates of one scene; compare them to each other, not against an absolute standard. Judge in this priority order:
1. subject sharpness where it matters (the eyes), judged on the crops when given;
2. eyes and expression (open, engaged, natural; not mid-blink or mid-word);
3. gesture and moment;
4. composition and background (framing, horizon, distractions at the edges, cropped limbs);
5. exposure only if it cannot be fixed; fixable exposure never counts against a frame.
Return every frame exactly once, best first.`

// RankSchema is the rank call's JSON Schema.
func RankSchema() map[string]any {
	return rankSchema
}

var rankSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"ranking", "summary"},
	"properties": map[string]any{
		"ranking": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"frame", "strength", "weakness"},
				"properties": map[string]any{
					"frame":    map[string]any{"type": "integer", "minimum": 1},
					"strength": map[string]any{"type": "string"},
					"weakness": map[string]any{"type": "string"},
				},
			},
		},
		"summary": map[string]any{"type": "string"},
	},
}

// RankRequest builds the rank call for a set of near-duplicate frames.
func RankRequest(frames []RankFrame, maxTokens int) llm.Request {
	var parts []llm.Part
	for i, f := range frames {
		n := i + 1
		parts = append(parts, llm.Text(fmt.Sprintf("Frame %d: full frame", n)), llm.JPEG(f.Full))
		if f.Crop != nil {
			parts = append(parts, llm.Text(fmt.Sprintf("Frame %d: subject at native resolution", n)), llm.JPEG(f.Crop))
		}
	}
	parts = append(parts, llm.Text(fmt.Sprintf("Rank these %d frames.", len(frames))))
	return llm.Request{
		System:     rankPrompt,
		Parts:      parts,
		SchemaName: "ranking",
		Schema:     rankSchema,
		MaxTokens:  maxTokens,
	}
}

// errNotPermutation is a decoded ranking that isn't a permutation of 1..n.
var errNotPermutation = errors.New("ranking is not a permutation of the input frames")

// DecodeRank parses a rank answer and checks it names every frame 1..n
// exactly once.
func DecodeRank(raw []byte, n int) (*Ranking, error) {
	var r Ranking
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode ranking: %w", err)
	}
	if len(r.Ranking) != n {
		return nil, errNotPermutation
	}
	seen := make(map[int]bool, n)
	for _, e := range r.Ranking {
		if e.Frame < 1 || e.Frame > n || seen[e.Frame] {
			return nil, errNotPermutation
		}
		seen[e.Frame] = true
	}
	return &r, nil
}

// Rank asks the backend to rank a set of near-duplicate frames, best first.
// It retries once if the answer isn't a permutation of the input frames (the
// schema mismatch retry already lives in the backend). Like Evaluate, a
// result delivered together with llm.ErrQuotaStop is returned with that
// error.
func Rank(ctx context.Context, b llm.Backend, frames []RankFrame, maxTokens int) (*Ranking, Usage, error) {
	n := len(frames)
	req := RankRequest(frames, maxTokens)
	var total Usage
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := b.Call(ctx, req)
		if resp != nil {
			total.Add(resp.Usage)
		}
		if resp == nil || resp.JSON == nil {
			return nil, total, err
		}
		r, derr := DecodeRank(resp.JSON, n)
		if derr == nil {
			return r, total, err
		}
		if err != nil {
			if errors.Is(err, llm.ErrQuotaStop) {
				return nil, total, fmt.Errorf("%w; also, model output does not rank every frame once: %v", err, derr)
			}
			return nil, total, err
		}
		lastErr = derr
	}
	return nil, total, lastErr
}
