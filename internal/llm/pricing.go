package llm

import "errors"

// Price is a model's list price in USD per million tokens. Read is the cache-read
// price (0 = a tenth of In, the usual rate); cache writes (5-minute TTL) cost
// 1.25 × In.
type Price struct{ In, Out, Read float64 }

// anthropicPrices are first-party list prices: input, output, cache read (claude-api
// skill, cached 2026-09-25). Batches are billed at half. Update when prices change.
var anthropicPrices = map[string]Price{
	"claude-fable-5-1":          {10, 50, 0.25},
	"claude-fable-5":            {10, 50, 1},
	"claude-opus-5-5":           {4, 20, 0.2},
	"claude-opus-5":             {5, 25, 0.5},
	"claude-opus-4-8":           {5, 25, 0.5},
	"claude-opus-4-7":           {5, 25, 0.5},
	"claude-opus-4-6":           {5, 25, 0.5},
	"claude-sonnet-5-5":         {2, 10, 0.2},
	"claude-sonnet-5":           {2, 10, 0.2},
	"claude-sonnet-4-6":         {3, 15, 0.3},
	"claude-haiku-4-5":          {1, 5, 0.1},
	"claude-haiku-4-5-20251001": {1, 5, 0.1},
}

// PriceFor returns the per-token price when calls are billed per token: only the
// anthropic backend. claude-code runs on a subscription and openai-compatible
// servers are usually local, so they have no price.
func PriceFor(backend, model string) (Price, bool) {
	if backend != "anthropic" {
		return Price{}, false
	}
	p, ok := anthropicPrices[model]
	return p, ok
}

// Cost prices one usage; batch halves it.
func (p Price) Cost(u Usage, batch bool) float64 {
	read := p.Read
	if read == 0 {
		read = p.In / 10
	}
	c := (float64(u.InputTokens)*p.In + float64(u.CacheWriteTokens)*p.In*1.25 +
		float64(u.CacheReadTokens)*read + float64(u.OutputTokens)*p.Out) / 1e6
	if batch {
		c /= 2
	}
	return c
}

// Per-frame token use measured on real M11-P runs (2026-09): about 6k input and
// 1k output for the evaluation (images dominate; Sonnet 5 thinks a little), plus a
// share of locate calls for frames without a detected face: ~7k in at a 1568 px
// full frame. The 1024 px default (2026-10-01) measured 16% less input.
const (
	estInPerFrame  = 6_000
	estOutPerFrame = 1_000
)

// Estimate is a rough list-price projection for n frames.
func Estimate(n int, p Price, batch bool) (usd float64, in, out int) {
	in, out = n*estInPerFrame, n*estOutPerFrame
	return p.Cost(Usage{InputTokens: in, OutputTokens: out}, batch), in, out
}

// Per-call rank token use assumed for estimates: a set's full frames and subject
// crops (up to 8 per call) fit comfortably under 10k input tokens; 1k output
// covers the ranking, notes, and summary.
const (
	estRankInPerCall  = 10_000
	estRankOutPerCall = 1_000
)

// EstimateRank is a rough list-price projection for a number of rank calls
// (chunk calls plus final calls).
func EstimateRank(calls int, p Price, batch bool) (usd float64, in, out int) {
	in, out = calls*estRankInPerCall, calls*estRankOutPerCall
	return p.Cost(Usage{InputTokens: in, OutputTokens: out}, batch), in, out
}

// ErrBudget stops dispatch once a run's cost reaches --max-cost; finished
// results are kept and --resume continues later.
var ErrBudget = errors.New("cost budget reached")
