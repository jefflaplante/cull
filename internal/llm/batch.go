package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrRejected marks a definitive refusal (a 4xx other than 408/429): the request
// was not accepted, so nothing was created and it is safe to try again later.
var ErrRejected = errors.New("request rejected")

// BatchRequest is one request of a Message Batch; CustomID matches results
// (1–64 of [A-Za-z0-9_-]).
type BatchRequest struct {
	CustomID string
	Req      Request
}

// BatchStatus is a batch's progress.
type BatchStatus struct {
	ID         string
	Ended      bool
	Counts     map[string]int // processing, succeeded, errored, canceled, expired
	ResultsURL string
}

// BatchResult is one request's outcome. Err is set for errored, canceled or
// expired items and for output that doesn't match the request's schema.
type BatchResult struct {
	CustomID string
	Response *Response
	Err      error
}

func (a *Anthropic) batchesURL() string {
	return strings.TrimSuffix(a.Endpoint, "/messages") + "/messages/batches"
}

// SubmitBatch creates a Message Batch (half price, asynchronous) and returns its ID.
func (a *Anthropic) SubmitBatch(ctx context.Context, reqs []BatchRequest) (string, error) {
	items := make([]map[string]any, len(reqs))
	for i, r := range reqs {
		items[i] = map[string]any{"custom_id": r.CustomID, "params": a.params(r.Req)}
	}
	body, err := json.Marshal(map[string]any{"requests": items})
	if err != nil {
		return "", err
	}
	raw, err := a.do(ctx, http.MethodPost, a.batchesURL(), body, true)
	if err != nil {
		return "", err
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &b); err != nil || b.ID == "" {
		return "", fmt.Errorf("batch create: unexpected response %s", truncate(raw, 200))
	}
	return b.ID, nil
}

// BatchStatus retrieves a batch.
func (a *Anthropic) BatchStatus(ctx context.Context, id string) (BatchStatus, error) {
	raw, err := a.do(ctx, http.MethodGet, a.batchesURL()+"/"+id, nil, false)
	if err != nil {
		return BatchStatus{}, err
	}
	var b struct {
		ID               string         `json:"id"`
		ProcessingStatus string         `json:"processing_status"`
		RequestCounts    map[string]int `json:"request_counts"`
		ResultsURL       string         `json:"results_url"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return BatchStatus{}, fmt.Errorf("batch status: %w", err)
	}
	return BatchStatus{ID: b.ID, Ended: b.ProcessingStatus == "ended", Counts: b.RequestCounts, ResultsURL: b.ResultsURL}, nil
}

// BatchResults streams an ended batch's results (in any order) to fn, validating
// each succeeded item against schemaFor(custom_id).
func (a *Anthropic) BatchResults(ctx context.Context, resultsURL string, schemaFor func(string) map[string]any, fn func(BatchResult)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultsURL, nil)
	if err != nil {
		return err
	}
	a.headers(req)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("batch results: status %d: %s", resp.StatusCode, truncate(b, 300))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var item struct {
			CustomID string `json:"custom_id"`
			Result   struct {
				Type    string          `json:"type"`
				Message json.RawMessage `json:"message"`
				Error   struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				} `json:"error"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &item); err != nil {
			return fmt.Errorf("batch results: %w", err)
		}
		r := BatchResult{CustomID: item.CustomID}
		switch item.Result.Type {
		case "succeeded":
			resp, err := parseAnthropic(item.Result.Message)
			r.Response, r.Err = resp, err
			if err == nil {
				if verr := Validate(schemaFor(item.CustomID), resp.JSON); verr != nil {
					r.Err = fmt.Errorf("model output does not match schema: %w", verr)
				}
			}
		case "errored":
			r.Err = fmt.Errorf("batch item errored: %s: %s", item.Result.Error.Error.Type, item.Result.Error.Error.Message)
		default:
			r.Err = fmt.Errorf("batch item %s", item.Result.Type)
		}
		fn(r)
	}
	return sc.Err()
}

func (a *Anthropic) headers(req *http.Request) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

// do is one JSON call with the messages retry policy (429/529/5xx and connection
// errors, honouring retry-after). A create is not idempotent: it is retried only
// on 429/529, which mean nothing was accepted; after a connection error or 5xx
// the batch may exist, so the error is returned for the caller to treat as
// "outcome unknown". Definitive rejections wrap ErrRejected.
func (a *Anthropic) do(ctx context.Context, method, url string, body []byte, create bool) ([]byte, error) {
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
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		a.headers(req)
		resp, err := a.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if create {
				return nil, err
			}
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return raw, nil
		case resp.StatusCode == 429 || resp.StatusCode == 529 || (resp.StatusCode >= 500 && !create):
			lastErr = fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(raw, 300))
			retryAfter = 0
			if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil {
				retryAfter = time.Duration(s) * time.Second
			}
		case resp.StatusCode >= 500 || resp.StatusCode == 408:
			return nil, fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(raw, 500))
		default:
			return nil, fmt.Errorf("%w: api status %d: %s", ErrRejected, resp.StatusCode, truncate(raw, 500))
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", a.MaxRetries+1, lastErr)
}
