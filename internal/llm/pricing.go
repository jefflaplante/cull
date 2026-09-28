package llm

import "errors"

// Price is a model's list price in USD per million tokens.
type Price struct{ In, Out float64 }

// anthropicPrices are first-party list prices (claude-api skill, cached
// 2026-06-24). Batches are billed at half. Update when prices change.
var anthropicPrices = map[string]Price{
	"claude-fable-5-1":  {10, 50},
	"claude-fable-5":    {10, 50},
	"claude-opus-5-5":   {4, 20},
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-opus-4-7":   {5, 25},
	"claude-opus-4-6":   {5, 25},
	"claude-sonnet-5":   {2, 10},
	"claude-sonnet-4-6": {3, 15},
	"claude-haiku-4-5":  {1, 5},
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
	c := (float64(u.InputTokens)*p.In + float64(u.OutputTokens)*p.Out) / 1e6
	if batch {
		c /= 2
	}
	return c
}

// Per-frame token use measured on real M11-P runs (2026-09): about 6k input and
// 1k output for the evaluation (images dominate; Sonnet 5 thinks a little), plus a
// share of locate calls for frames without a detected face.
const (
	estInPerFrame  = 7_000
	estOutPerFrame = 1_000
)

// Estimate is a rough list-price projection for n frames.
func Estimate(n int, p Price, batch bool) (usd float64, in, out int) {
	in, out = n*estInPerFrame, n*estOutPerFrame
	return p.Cost(Usage{InputTokens: in, OutputTokens: out}, batch), in, out
}

// ErrBudget stops dispatch once a run's cost reaches --max-cost; finished
// results are kept and --resume continues later.
var ErrBudget = errors.New("cost budget reached")
