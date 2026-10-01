package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicEndpoint = "https://api.anthropic.com/v1/messages"
	anthropicVersion  = "2023-06-01"
	// Sonnet 5 and 5.5 run adaptive thinking by default; thinking shares max_tokens with
	// the answer, so the floor is well above the answer's own size. Unused
	// headroom costs nothing, while hitting the cap wastes the whole call.
	anthropicMinTokens = 8192
)

// Anthropic calls the Messages API with structured JSON output
// (output_config.format), which works on models that reject forced tool use.
type Anthropic struct {
	APIKey     string
	Model      string
	Endpoint   string
	MaxRetries int
	HTTP       *http.Client
}

func NewAnthropic(apiKey, model string) *Anthropic {
	return &Anthropic{
		APIKey:     apiKey,
		Model:      model,
		Endpoint:   anthropicEndpoint,
		MaxRetries: 5,
		HTTP:       &http.Client{Timeout: 180 * time.Second},
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Call(ctx context.Context, req Request) (*Response, error) {
	payload, err := a.body(req)
	if err != nil {
		return nil, err
	}
	return validated(ctx, req.Schema, func(ctx context.Context) (*Response, error) { return a.post(ctx, payload) })
}

func (a *Anthropic) body(req Request) ([]byte, error) { return json.Marshal(a.params(req)) }

// params is the Messages API request body; batches embed it per request.
func (a *Anthropic) params(req Request) map[string]any {
	content := make([]map[string]any, 0, len(req.Parts))
	for _, p := range req.Parts {
		if p.JPEG != nil {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(p.JPEG),
			}})
		} else {
			content = append(content, map[string]any{"type": "text", "text": p.Text})
		}
	}
	oc := map[string]any{"format": map[string]any{"type": "json_schema", "schema": Portable(req.Schema)}}
	if req.Effort != "" {
		oc["effort"] = req.Effort
	}
	return map[string]any{
		"model":      a.Model,
		"max_tokens": max(req.MaxTokens, anthropicMinTokens),
		// The system prompt (and the output format after it) is the same for every
		// frame of a run: cached, the frames after the first read it at a tenth of
		// the input price. Below the model's minimum (512-1024 tokens) it silently
		// isn't cached, which costs nothing.
		"system":        []any{map[string]any{"type": "text", "text": req.System, "cache_control": map[string]any{"type": "ephemeral"}}},
		"messages":      []any{map[string]any{"role": "user", "content": content}},
		"output_config": oc,
	}
}

func (a *Anthropic) post(ctx context.Context, payload []byte) (*Response, error) {
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt <= a.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff(attempt, retryAfter)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := a.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return parseAnthropic(body)
		case resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(body, 300))
			retryAfter = 0
			if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil {
				retryAfter = time.Duration(s) * time.Second
			}
		default: // other 4xx: not retryable
			return nil, fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(body, 500))
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", a.MaxRetries+1, lastErr)
}

func parseAnthropic(body []byte) (*Response, error) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      Usage  `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	switch r.StopReason {
	case "refusal":
		return &Response{Usage: r.Usage}, fmt.Errorf("model refused (stop_reason=refusal)")
	case "max_tokens":
		return &Response{Usage: r.Usage}, fmt.Errorf("output truncated (stop_reason=max_tokens)")
	}
	for _, b := range r.Content {
		if b.Type == "text" {
			return &Response{JSON: json.RawMessage(strings.TrimSpace(b.Text)), Usage: r.Usage}, nil
		}
	}
	return &Response{Usage: r.Usage}, fmt.Errorf("no text block in response (stop_reason=%s)", r.StopReason)
}
