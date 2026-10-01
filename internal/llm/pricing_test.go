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
	if in != 600_000 || out != 100_000 || math.Abs(usd-2.2) > 1e-9 {
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

func TestCurrentModelsArePriced(t *testing.T) {
	for _, m := range []string{"claude-sonnet-5-5", "claude-sonnet-5", "claude-opus-5-5", "claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-fable-5-1"} {
		if _, ok := PriceFor("anthropic", m); !ok {
			t.Errorf("%s unpriced", m)
		}
	}
	if p, _ := PriceFor("anthropic", "claude-sonnet-5-5"); p != (Price{2, 10, 0.2}) {
		t.Errorf("sonnet 5.5 = %+v", p)
	}
}

func TestCostPricesCacheTokens(t *testing.T) {
	p, _ := PriceFor("anthropic", "claude-sonnet-5-5")
	u := Usage{InputTokens: 1e6, OutputTokens: 1e6, CacheWriteTokens: 1e6, CacheReadTokens: 1e6}
	if got := p.Cost(u, false); math.Abs(got-(2+10+2.5+0.2)) > 1e-9 {
		t.Fatalf("cost %v", got)
	}
	if o, _ := PriceFor("anthropic", "claude-opus-5-5"); o.Read != 0.2 {
		t.Fatalf("opus 5.5 read %v", o.Read)
	}
	if got := (Price{In: 3, Out: 15}).Cost(Usage{CacheReadTokens: 1e6}, false); math.Abs(got-0.3) > 1e-9 {
		t.Fatalf("default read is a tenth of input: %v", got)
	}
}

func TestUsageAddSumsCache(t *testing.T) {
	u := Usage{InputTokens: 1, CacheWriteTokens: 2, CacheReadTokens: 3}
	u.Add(Usage{InputTokens: 1, CacheWriteTokens: 2, CacheReadTokens: 3, OutputTokens: 4})
	if u != (Usage{InputTokens: 2, OutputTokens: 4, CacheWriteTokens: 4, CacheReadTokens: 6}) {
		t.Fatalf("%+v", u)
	}
}
