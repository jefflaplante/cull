package cli

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/jefflaplante/gophotocull/internal/eval"
)

// policyFlags are the decision knobs shared by cull and decide. New signals
// default to review; nothing is culled on them unless asked.
type policyFlags struct {
	minCropArea          float64
	reviewBelowSharpness float64
	eyesClosed           string
	duplicates           string
	rawClipped           string
	rawClipThreshold     float64
}

func (pf *policyFlags) register(f *pflag.FlagSet) {
	f.Float64Var(&pf.minCropArea, "min-crop-area", 0.6, "reject suggested crops retaining less than this fraction of the frame")
	f.Float64Var(&pf.reviewBelowSharpness, "review-below-sharpness", 0, "send frames whose sharpness score is below this to review (0 = off)")
	f.StringVar(&pf.eyesClosed, "eyes-closed", "review", "what to do with closed eyes: ignore, review, or cull")
	f.StringVar(&pf.duplicates, "duplicates", "review", "what to do with non-best frames of a burst: ignore, review, or cull")
	f.StringVar(&pf.rawClipped, "raw-clipped", "review", "what to do when the raw's highlights are clipped: ignore, review, or cull")
	f.Float64Var(&pf.rawClipThreshold, "raw-clip-threshold", 0.5, "percent of raw samples at the white level that counts as clipped")
}

func (pf *policyFlags) policy() (eval.Policy, error) {
	if pf.minCropArea <= 0 || pf.minCropArea > 1 {
		return eval.Policy{}, fmt.Errorf("--min-crop-area must be in (0, 1]")
	}
	if pf.reviewBelowSharpness < 0 || pf.reviewBelowSharpness > 10 {
		return eval.Policy{}, fmt.Errorf("--review-below-sharpness must be in [0, 10]")
	}
	eyes, err := eval.ParseAction(pf.eyesClosed)
	if err != nil {
		return eval.Policy{}, fmt.Errorf("--eyes-closed %w", err)
	}
	dups, err := eval.ParseAction(pf.duplicates)
	if err != nil {
		return eval.Policy{}, fmt.Errorf("--duplicates %w", err)
	}
	raw, err := eval.ParseAction(pf.rawClipped)
	if err != nil {
		return eval.Policy{}, fmt.Errorf("--raw-clipped %w", err)
	}
	if pf.rawClipThreshold <= 0 || pf.rawClipThreshold > 100 {
		return eval.Policy{}, fmt.Errorf("--raw-clip-threshold must be in (0, 100]")
	}
	return eval.Policy{
		MinCropArea: pf.minCropArea, ReviewBelowSharpness: pf.reviewBelowSharpness, EyesClosed: eyes, Duplicates: dups,
		RawClipped: raw, RawClipThreshold: pf.rawClipThreshold,
	}, nil
}
