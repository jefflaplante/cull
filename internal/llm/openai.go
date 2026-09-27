package llm

import (
	"bufio"
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
	// Streaming hangs up as soon as the JSON closes, so a generous cap costs
	// nothing and leaves room for models that think before answering.
	openaiStreamMaxTokens  = 8192
	openaiDefaultMaxTokens = 1024
)

// OpenAI calls any OpenAI-compatible /chat/completions endpoint (LM Studio, vLLM,
// hosted services) with json_schema response_format. Some local servers keep
// generating after the JSON is complete until max_tokens; in Stream mode the
// client hangs up the moment the top-level object closes.
type OpenAI struct {
	BaseURL    string // e.g. http://127.0.0.1:8000/v1
	APIKey     string // optional; sent as a Bearer token when set
	Model      string
	Stream     bool
	MaxRetries int
	HTTP       *http.Client
}

func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		Stream:     true,
		MaxRetries: 5,
		HTTP:       &http.Client{Timeout: 600 * time.Second}, // local models on big payloads are slow
	}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Call(ctx context.Context, req Request) (*Response, error) {
	payload, err := o.body(req)
	if err != nil {
		return nil, err
	}
	return validated(ctx, req.Schema, func(ctx context.Context) (*Response, error) { return o.post(ctx, payload) })
}

func (o *OpenAI) body(req Request) ([]byte, error) {
	content := make([]map[string]any, 0, len(req.Parts))
	for _, p := range req.Parts {
		if p.JPEG != nil {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(p.JPEG),
			}})
		} else {
			content = append(content, map[string]any{"type": "text", "text": p.Text})
		}
	}
	name := req.SchemaName
	if name == "" {
		name = "result"
	}
	body := map[string]any{
		"model": o.Model,
		"messages": []any{
			map[string]any{"role": "system", "content": req.System},
			map[string]any{"role": "user", "content": content},
		},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": name, "strict": true, "schema": Portable(req.Schema),
		}},
		"temperature": 0,
	}
	if o.Stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
		body["max_tokens"] = openaiStreamMaxTokens
	} else {
		mt := req.MaxTokens
		if mt <= 0 {
			mt = openaiDefaultMaxTokens
		}
		body["max_tokens"] = mt
	}
	return json.Marshal(body)
}

func (o *OpenAI) post(ctx context.Context, payload []byte) (*Response, error) {
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt <= o.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff(attempt, retryAfter)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := o.once(ctx, payload)
		var se *statusError
		switch {
		case err == nil:
			return resp, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case asStatus(err, &se) && (se.code == 429 || se.code >= 500):
			lastErr, retryAfter = err, se.retryAfter
		case asStatus(err, &se): // other HTTP errors: not retryable
			return nil, err
		case isTransport(err):
			lastErr, retryAfter = err, 0
		default: // malformed/incomplete output: validated() decides whether to retry
			return resp, err
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", o.MaxRetries+1, lastErr)
}

type statusError struct {
	code       int
	retryAfter time.Duration
	body       string
}

func (e *statusError) Error() string { return fmt.Sprintf("api status %d: %s", e.code, e.body) }

func asStatus(err error, target **statusError) bool {
	se, ok := err.(*statusError)
	if ok {
		*target = se
	}
	return ok
}

type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isTransport(err error) bool { _, ok := err.(*transportError); return ok }

func (o *OpenAI) once(ctx context.Context, payload []byte) (*Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // hanging up is how streaming stops a server that won't stop itself
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, &transportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		se := &statusError{code: resp.StatusCode, body: truncate(body, 500)}
		if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil {
			se.retryAfter = time.Duration(s) * time.Second
		}
		return nil, se
	}
	if o.Stream {
		return readStream(resp.Body)
	}
	return readCompletion(resp.Body)
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *openaiUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

func readCompletion(r io.Reader) (*Response, error) {
	var c struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return nil, &transportError{fmt.Errorf("decode response: %w", err)}
	}
	if len(c.Choices) == 0 {
		return nil, fmt.Errorf("response has no choices")
	}
	obj, ok := FirstJSONObject(c.Choices[0].Message.Content)
	if !ok {
		return &Response{Usage: c.Usage.toUsage()}, incomplete(c.Choices[0].Message.Content)
	}
	return &Response{JSON: json.RawMessage(obj), Usage: c.Usage.toUsage()}, nil
}

// readStream accumulates SSE deltas and returns as soon as a complete top-level
// JSON object has arrived; the caller's deferred cancel then drops the connection.
func readStream(r io.Reader) (*Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var acc strings.Builder
	var usage Usage
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *openaiUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // tolerate keep-alives and vendor extensions
		}
		if chunk.Usage != nil {
			usage = chunk.Usage.toUsage()
		}
		if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == "" {
			continue
		}
		acc.WriteString(chunk.Choices[0].Delta.Content)
		if obj, ok := FirstJSONObject(acc.String()); ok {
			return &Response{JSON: json.RawMessage(obj), Usage: usage}, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &transportError{err}
	}
	return &Response{Usage: usage}, incomplete(acc.String())
}
