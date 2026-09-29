package cli

import (
	"fmt"
	"io"
	"strings"

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
	p := eval.Policy{
		MinCropArea: pf.minCropArea, ReviewBelowSharpness: pf.reviewBelowSharpness, EyesClosed: eyes, Outranked: outranked,
		RawClipped: raw, RawClipThreshold: pf.rawClipThreshold, KeepBest: pf.keepBest,
	}
	return p, validPolicy(p)
}

// validPolicy checks a policy whether it came from flags or from a report (which a
// person may have edited by hand).
func validPolicy(p eval.Policy) error {
	if p.MinCropArea <= 0 || p.MinCropArea > 1 {
		return fmt.Errorf("--min-crop-area must be in (0, 1]")
	}
	if p.ReviewBelowSharpness < 0 || p.ReviewBelowSharpness > 10 {
		return fmt.Errorf("--review-below-sharpness must be in [0, 10]")
	}
	for _, a := range []struct {
		flag string
		a    eval.Action
	}{{"--eyes-closed", p.EyesClosed}, {"--outranked", p.Outranked}, {"--raw-clipped", p.RawClipped}} {
		if _, err := eval.ParseAction(string(a.a)); err != nil {
			return fmt.Errorf("%s %w", a.flag, err)
		}
	}
	if p.RawClipThreshold <= 0 || p.RawClipThreshold > 100 {
		return fmt.Errorf("--raw-clip-threshold must be in (0, 100]")
	}
	if p.KeepBest < 0 || p.KeepBest > 5 {
		return fmt.Errorf("--keep-best must be in [0, 5]: at 6 or more, a chunked set's final ranking round could exceed the 8-frames-per-call limit")
	}
	return nil
}

// resolve is the policy for a command working on an existing report: the report's
// stored policy (saved is nil in reports from before it was stored), with each flag
// typed on the command line overriding its own setting. notes names the stored
// settings in effect that differ from the flag defaults, as flags ("--keep-best 2"),
// so the command can say it is still using them.
func (pf *policyFlags) resolve(fs *pflag.FlagSet, saved *eval.Policy) (eval.Policy, []string, error) {
	p, err := pf.policy()
	if err != nil || saved == nil {
		return p, nil, err
	}
	fields := []struct {
		flag string
		dst  any
		src  any
	}{
		{"min-crop-area", &p.MinCropArea, saved.MinCropArea},
		{"review-below-sharpness", &p.ReviewBelowSharpness, saved.ReviewBelowSharpness},
		{"eyes-closed", &p.EyesClosed, saved.EyesClosed},
		{"outranked", &p.Outranked, saved.Outranked},
		{"raw-clipped", &p.RawClipped, saved.RawClipped},
		{"raw-clip-threshold", &p.RawClipThreshold, saved.RawClipThreshold},
		{"keep-best", &p.KeepBest, saved.KeepBest},
	}
	var notes []string
	for _, f := range fields {
		fl := fs.Lookup(f.flag)
		if fl == nil || fl.Changed {
			continue
		}
		switch d := f.dst.(type) {
		case *float64:
			*d = f.src.(float64)
		case *int:
			*d = f.src.(int)
		case *eval.Action:
			*d = f.src.(eval.Action)
		}
		if v := fmt.Sprint(f.src); v != fl.DefValue {
			notes = append(notes, "--"+f.flag+" "+v)
		}
	}
	if err := validPolicy(p); err != nil {
		return p, nil, fmt.Errorf("the report's stored policy: %w", err)
	}
	return p, notes, nil
}

// noteStoredPolicy tells the user which stored settings a command is still using, so
// tuning from an earlier decide is visible rather than a silent surprise.
func noteStoredPolicy(w io.Writer, notes []string) {
	if len(notes) > 0 {
		fmt.Fprintf(w, "policy from the report: %s (a flag on the command line overrides it)\n", strings.Join(notes, " "))
	}
}
