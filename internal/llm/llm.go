// Package llm is the provider-neutral structured call every model request goes
// through: a system prompt, ordered text and JPEG parts, and a JSON Schema in;
// schema-validated JSON, token usage, and (for subscriptions) quota out.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Part is one element of the user message: exactly one of Text or JPEG is set.
type Part struct {
	Text string
	JPEG []byte
}

func Text(s string) Part { return Part{Text: s} }
func JPEG(b []byte) Part { return Part{JPEG: b} }

type Request struct {
	System     string
	Parts      []Part
	SchemaName string         // identifies the call, e.g. "evaluation", "focus_target"
	Schema     map[string]any // JSON Schema; every object sets additionalProperties:false
	MaxTokens  int            // 0 = backend default
	Effort     string         // low|medium|high|xhigh|max; "" = the model's default
}

// Usage is a call's tokens. InputTokens is the uncached remainder only (the
// Messages API's input_tokens); cache writes and reads are counted apart because
// they are priced apart. Backends that don't report caching leave them 0.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
}

func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CacheReadTokens += o.CacheReadTokens
}

// Sub is u less o, field by field.
func (u Usage) Sub(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens - o.InputTokens, OutputTokens: u.OutputTokens - o.OutputTokens,
		CacheWriteTokens: u.CacheWriteTokens - o.CacheWriteTokens, CacheReadTokens: u.CacheReadTokens - o.CacheReadTokens}
}

// TotalIn is every input token the call processed: uncached, written and read.
func (u Usage) TotalIn() int { return u.InputTokens + u.CacheWriteTokens + u.CacheReadTokens }

// ModelPinner is a backend whose model name is an alias resolved per call
// (claude-code's "sonnet"): Resolved reports what it resolved to, and Pin makes a
// different resolution an ErrAbortRun.
type ModelPinner interface {
	Resolved() string
	Pin(model string)
}

// Quota is subscription utilization as reported by Claude Code (0..1 per window).
type Quota struct {
	Status             string
	FiveHour, SevenDay float64
}

type Response struct {
	JSON  json.RawMessage // validated against Request.Schema
	Usage Usage
	Quota *Quota
}

type Backend interface {
	Name() string
	Call(ctx context.Context, req Request) (*Response, error)
}

// ErrQuotaStop is returned together with a valid Response when the subscription
// quota crossed the configured threshold: keep this result, dispatch no more.
var ErrQuotaStop = errors.New("subscription quota threshold reached")

// ErrAbortRun means continuing could do harm (e.g. billing the wrong account);
// the caller must stop the whole run, not just this frame.
var ErrAbortRun = errors.New("aborting run")

// malformedError is model output that never became a complete JSON object (a
// local model looping until max_tokens, say). Like a schema mismatch, it is the
// model's answer, not the transport, so it earns one fresh attempt.
type malformedError struct{ msg string }

func (e *malformedError) Error() string { return e.msg }

func incomplete(output string) error {
	flat := strings.Join(strings.Fields(output), " ")
	if len(flat) > 160 {
		flat = "..." + flat[len(flat)-160:]
	}
	return &malformedError{fmt.Sprintf("incomplete JSON in model output (%d chars, ends: %s)", len(output), flat)}
}

// validated runs attempt, checks the JSON against schema, and makes one fresh
// attempt if it doesn't conform or never completed. Usage from both attempts is
// summed, and returned in a non-nil Response on every error path too. Call errors
// are returned as-is (retrying transport failures is the backend's job);
// ErrQuotaStop is passed through alongside a validated response.
func validated(ctx context.Context, schema map[string]any, attempt func(context.Context) (*Response, error)) (*Response, error) {
	var total Usage
	var lastErr error
	for i := 0; i < 2; i++ {
		resp, err := attempt(ctx)
		if resp != nil {
			total.Add(resp.Usage)
		}
		var bad *malformedError
		if errors.As(err, &bad) {
			lastErr = err
			continue
		}
		if err != nil && !errors.Is(err, ErrQuotaStop) {
			if resp == nil {
				resp = &Response{}
			}
			resp.Usage = total
			return resp, err
		}
		if resp == nil {
			return &Response{Usage: total}, fmt.Errorf("backend returned no response")
		}
		if verr := Validate(schema, resp.JSON); verr != nil {
			if err != nil { // quota stop: don't spend another call, and keep the stop signal
				return &Response{Usage: total}, fmt.Errorf("%w; also, model output does not match schema: %v", err, verr)
			}
			lastErr = verr
			continue
		}
		resp.Usage = total
		return resp, err
	}
	// Both attempts were paid for: their usage rides on the error.
	return &Response{Usage: total}, fmt.Errorf("model output does not match schema: %w", lastErr)
}

func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(d/2)+1)) // jitter
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
