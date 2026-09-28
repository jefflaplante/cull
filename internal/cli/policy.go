package cli

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/jefflaplante/gophotocull/internal/eval"
)

// policyFlags are the decision knobs shared by judge, rank, and decide. New
// signals default to review; nothing is culled on them unless asked.
type policyFlags struct {
	minCropArea          float64
	reviewBelowSharpness float64
	eyesClosed           string
	outranked            string
	rawClipped           string
	rawClipThreshold     float64
	keepBest             int
}

func (pf *policyFlags) register(f *pflag.FlagSet) {
	f.Float64Var(&pf.minCropArea, "min-crop-area", 0.6, "reject suggested crops retaining less than this fraction of the frame")
	f.Float64Var(&pf.reviewBelowSharpness, "review-below-sharpness", 0, "send frames whose sharpness score is below this to review (0 = off)")
	f.StringVar(&pf.eyesClosed, "eyes-closed", "review", "what to do with closed eyes: ignore, review, or cull")
	f.StringVar(&pf.outranked, "outranked", "review", "what to do with frames ranked below --keep-best in their set: ignore, review, or cull")
	f.StringVar(&pf.rawClipped, "raw-clipped", "review", "what to do when the raw's highlights are clipped: ignore, review, or cull")
	f.Float64Var(&pf.rawClipThreshold, "raw-clip-threshold", 0.5, "percent of raw samples at the white level that counts as clipped")
	f.IntVar(&pf.keepBest, "keep-best", 3, "per set, keep this many best-ranked frames (0-5: a chunked final ranking round tops out at 8 frames)")
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
	outranked, err := eval.ParseAction(pf.outranked)
	if err != nil {
		return eval.Policy{}, fmt.Errorf("--outranked %w", err)
	}
	raw, err := eval.ParseAction(pf.rawClipped)
	if err != nil {
		return eval.Policy{}, fmt.Errorf("--raw-clipped %w", err)
	}
	if pf.rawClipThreshold <= 0 || pf.rawClipThreshold > 100 {
		return eval.Policy{}, fmt.Errorf("--raw-clip-threshold must be in (0, 100]")
	}
	if pf.keepBest < 0 || pf.keepBest > 5 {
		return eval.Policy{}, fmt.Errorf("--keep-best must be in [0, 5]: at 6 or more, a chunked set's final ranking round could exceed the 8-frames-per-call limit")
	}
	return eval.Policy{
		MinCropArea: pf.minCropArea, ReviewBelowSharpness: pf.reviewBelowSharpness, EyesClosed: eyes, Outranked: outranked,
		RawClipped: raw, RawClipThreshold: pf.rawClipThreshold, KeepBest: pf.keepBest,
	}, nil
}
