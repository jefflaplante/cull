package llm

import (
	"math"
	"testing"
)

func TestPricing(t *testing.T) {
	p, ok := PriceFor("anthropic", "claude-sonnet-5")
	if !ok || p.In != 2 || p.Out != 10 {
		t.Fatalf("sonnet 5: %+v %v", p, ok)
	}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if got := p.Cost(u, false); got != 12 {
		t.Fatalf("cost %v", got)
	}
	if got := p.Cost(u, true); got != 6 {
		t.Fatalf("batch cost %v", got)
	}
	for _, c := range [][2]string{{"openai", "local-model"}, {"claude-code", "sonnet"}, {"anthropic", "claude-unknown-9"}} {
		if _, ok := PriceFor(c[0], c[1]); ok {
			t.Errorf("%v should have no per-token price", c)
		}
	}
	usd, in, out := Estimate(100, p, false)
	if in != 700_000 || out != 100_000 || math.Abs(usd-2.4) > 1e-9 {
		t.Fatalf("estimate: $%v in=%d out=%d", usd, in, out)
	}
}

func TestEstimateRank(t *testing.T) {
	p, _ := PriceFor("anthropic", "claude-sonnet-5")
	usd, in, out := EstimateRank(3, p, false)
	if in != 30_000 || out != 3_000 || math.Abs(usd-0.09) > 1e-9 {
		t.Fatalf("estimate rank: $%v in=%d out=%d", usd, in, out)
	}
	if usd2, _, _ := EstimateRank(3, p, true); math.Abs(usd2-usd/2) > 1e-9 {
		t.Fatalf("batch rank estimate should be half: %v vs %v", usd2, usd)
	}
}
