package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/llm"
)

type failingBackend struct{ calls int32 }

func (f *failingBackend) Name() string { return "failing" }
func (f *failingBackend) Call(context.Context, llm.Request) (*llm.Response, error) {
	f.calls++
	return nil, errors.New("upstream down")
}

func escalateCfg(dir string, esc llm.Backend) Config {
	c := moveCfg(dir)
	c.MoveCulled, c.Concurrency = false, 1
	c.Price = &llm.Price{In: 10_000} // $1 per 100 input tokens
	c.Escalate = &Escalation{Backend: esc, Model: "big", Price: &llm.Price{In: 20_000},
		On: map[string]bool{"missed_focus": true, "soft": true}}
	return c
}

func TestEscalationReevaluatesOnlyMatchingFrames(t *testing.T) {
	dir, primary := shoot(t) // L1 missed_focus, L2 sharp, L3 soft
	esc := &perFileBackend{} // answers sharp
	rep, _, err := Run(context.Background(), escalateCfg(dir, esc), primary)
	if err != nil {
		t.Fatal(err)
	}
	if esc.calls != 2 || rep.Escalation != "fake/big" {
		t.Fatalf("escalation calls=%d label=%q", esc.calls, rep.Escalation)
	}
	l1 := result(t, rep, "L1000001.DNG")
	// The first pass said missed_focus, the escalation sharp: they disagree, so
	// review rather than trusting either.
	if l1.Decision != eval.Review || l1.FirstPass == nil || l1.FirstPass.Evaluation.Sharpness.Status != "missed_focus" ||
		!strings.Contains(strings.Join(l1.Reasons, ";"), "disagree") {
		t.Fatalf("L1: decision=%s first=%+v", l1.Decision, l1.FirstPass)
	}
	if l1.Usage.InputTokens != 200 || l1.CostUSD != 3 { // $1 first pass + $2 escalation
		t.Fatalf("L1 usage=%+v cost=%v", l1.Usage, l1.CostUSD)
	}
	if l2 := result(t, rep, "L1000002.DNG"); l2.FirstPass != nil || l2.CostUSD != 1 {
		t.Fatalf("L2 escalated or mispriced: %+v %v", l2.FirstPass, l2.CostUSD)
	}
}

func TestEscalationFailureKeepsFirstPass(t *testing.T) {
	dir, primary := shoot(t)
	rep, _, err := Run(context.Background(), escalateCfg(dir, &failingBackend{}), primary)
	if err != nil {
		t.Fatal(err)
	}
	l1 := result(t, rep, "L1000001.DNG")
	if l1.Error != "" || l1.Decision != eval.Cull || !strings.Contains(strings.Join(l1.Fixups, ";"), "escalation failed") {
		t.Fatalf("L1: error=%q decision=%s fixups=%v", l1.Error, l1.Decision, l1.Fixups)
	}
}

func TestResumeRefusesDifferentEscalation(t *testing.T) {
	dir, primary := shoot(t)
	c := escalateCfg(dir, &perFileBackend{})
	if _, _, err := Run(context.Background(), c, primary); err != nil {
		t.Fatal(err)
	}
	c.Resume, c.Escalate = true, nil
	if _, _, err := Run(context.Background(), c, primary); err == nil || !strings.Contains(err.Error(), "-o for a separate report") {
		t.Fatalf("want refusal, got %v", err)
	}
}

// --effort applies to escalated evaluations too: they are evaluations, and the report
// records one effort for all of them.
func TestEscalationCarriesEffort(t *testing.T) {
	dir, primary := shoot(t)
	esc := &effortRecorder{}
	c := escalateCfg(dir, esc)
	c.Effort = "high"
	if _, _, err := Run(context.Background(), c, primary); err != nil {
		t.Fatal(err)
	}
	if esc.seen["evaluation"] != "high" {
		t.Fatalf("escalation saw effort %q", esc.seen["evaluation"])
	}
}
