package eval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jefflaplante/gophotocull/internal/llm"
)

// Usage is kept as an eval name so the report schema doesn't depend on llm.
type Usage = llm.Usage

// Labeled is an image plus the sentence that tells the model what it is.
type Labeled struct {
	Label string
	JPEG  []byte
}

// Input is one frame's worth of model inputs.
type Input struct {
	Filename    string
	FullFrame   []byte
	Subject     *Labeled // native-resolution crop of the intended focus target; nil if not located
	Landed      []Labeled
	StatsText   string
	MinCropArea float64
}

// evalMaxTokens bounds the answer itself; backends add headroom for thinking.
const evalMaxTokens = 1024

// EvalRequest builds the evaluation call. Exported so --save-inputs records
// exactly what is sent.
func EvalRequest(in Input) llm.Request {
	parts := []llm.Part{
		llm.Text("File: " + in.Filename + "\nFull frame, downscaled from the embedded preview:"),
		llm.JPEG(in.FullFrame),
	}
	if in.Subject != nil {
		parts = append(parts, llm.Text(in.Subject.Label), llm.JPEG(in.Subject.JPEG))
	} else {
		parts = append(parts, llm.Text("No subject crop: the intended focus target could not be located. "+
			"Judge sharpness from the full frame and the regions below, and say so in focus_target."))
	}
	for _, l := range in.Landed {
		parts = append(parts, llm.Text(l.Label), llm.JPEG(l.JPEG))
	}
	parts = append(parts, llm.Text("Measured statistics from the preview:\n"+in.StatsText))
	return llm.Request{
		System:     SystemPrompt(in.MinCropArea),
		Parts:      parts,
		SchemaName: "evaluation",
		Schema:     evaluationSchema,
		MaxTokens:  evalMaxTokens,
	}
}

// Evaluate asks the backend to assess one frame. A response delivered together
// with llm.ErrQuotaStop is still decoded and returned with that error.
func Evaluate(ctx context.Context, b llm.Backend, in Input) (*Evaluation, Usage, error) {
	resp, err := b.Call(ctx, EvalRequest(in))
	if resp == nil || resp.JSON == nil {
		if resp != nil {
			return nil, resp.Usage, err
		}
		return nil, Usage{}, err
	}
	var e Evaluation
	if uerr := json.Unmarshal(resp.JSON, &e); uerr != nil {
		return nil, resp.Usage, fmt.Errorf("decode evaluation: %w", uerr)
	}
	return &e, resp.Usage, err
}
