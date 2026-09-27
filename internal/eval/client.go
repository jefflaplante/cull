package eval

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultEndpoint  = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	toolName         = "record_evaluation"
)

// Client calls the Messages API. Structured output is obtained by forcing a single
// tool call whose input_schema is the evaluation schema.
type Client struct {
	APIKey     string
	Model      string
	MaxTokens  int
	MaxRetries int
	Endpoint   string
	HTTP       *http.Client
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (u *Usage) Add(o Usage) { u.InputTokens += o.InputTokens; u.OutputTokens += o.OutputTokens }

// Input is one image's worth of model inputs.
type Input struct {
	Filename    string
	FullFrame   []byte
	Tiles       [][]byte
	StatsText   string
	MinCropArea float64
}

func NewClient(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		MaxTokens:  1024,
		MaxRetries: 5,
		Endpoint:   defaultEndpoint,
		HTTP:       &http.Client{Timeout: 120 * time.Second},
	}
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func imageBlock(jpegData []byte) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(jpegData),
	}}
}

func (c *Client) buildRequest(in Input) ([]byte, error) {
	content := []map[string]any{
		textBlock("File: " + in.Filename + "\nFull frame (downscaled from the embedded preview):"),
		imageBlock(in.FullFrame),
	}
	if len(in.Tiles) > 0 {
		content = append(content, textBlock(fmt.Sprintf("%d highest-detail regions at native preview resolution (use these to judge focus):", len(in.Tiles))))
		for _, t := range in.Tiles {
			content = append(content, imageBlock(t))
		}
	}
	content = append(content, textBlock("Measured statistics from the preview:\n"+in.StatsText))

	return json.Marshal(map[string]any{
		"model":      c.Model,
		"max_tokens": c.MaxTokens,
		"system":     SystemPrompt(in.MinCropArea),
		"tools": []any{map[string]any{
			"name":         toolName,
			"description":  "Record the culling evaluation for this image.",
			"input_schema": toolSchema,
		}},
		"tool_choice": map[string]any{"type": "tool", "name": toolName},
		"messages":    []any{map[string]any{"role": "user", "content": content}},
	})
}

type apiResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
}

// Evaluate sends one image and returns the parsed evaluation.
func (c *Client) Evaluate(ctx context.Context, in Input) (*Evaluation, Usage, error) {
	payload, err := c.buildRequest(in)
	if err != nil {
		return nil, Usage{}, err
	}
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff(attempt, retryAfter)):
			case <-ctx.Done():
				return nil, Usage{}, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, Usage{}, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, Usage{}, ctx.Err()
			}
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return parseResponse(body)
		case resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(body, 300))
			retryAfter = 0
			if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil {
				retryAfter = time.Duration(s) * time.Second
			}
		default: // 4xx other than 429: not retryable
			return nil, Usage{}, fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(body, 500))
		}
	}
	return nil, Usage{}, fmt.Errorf("giving up after %d attempts: %w", c.MaxRetries+1, lastErr)
}

func parseResponse(body []byte) (*Evaluation, Usage, error) {
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, Usage{}, fmt.Errorf("decode response: %w", err)
	}
	for _, b := range r.Content {
		if b.Type == "tool_use" && b.Name == toolName {
			var e Evaluation
			if err := json.Unmarshal(b.Input, &e); err != nil {
				return nil, r.Usage, fmt.Errorf("decode tool input: %w", err)
			}
			return &e, r.Usage, nil
		}
	}
	return nil, r.Usage, fmt.Errorf("no %s tool call in response (stop_reason=%s)", toolName, r.StopReason)
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
