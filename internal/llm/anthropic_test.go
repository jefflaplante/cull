package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var tinySchema = map[string]any{
	"type": "object", "required": []string{"score"}, "additionalProperties": false,
	"properties": map[string]any{"score": map[string]any{"type": "number", "minimum": 0}},
}

func tinyRequest() Request {
	return Request{System: "sys", SchemaName: "t", Schema: tinySchema,
		Parts: []Part{Text("look"), JPEG([]byte{0xFF, 0xD8, 0xFF})}}
}

func anthropicReply(text, stop string) string {
	b, _ := json.Marshal(map[string]any{
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stop,
		"usage":       map[string]any{"input_tokens": 3000, "output_tokens": 200},
	})
	return string(b)
}

func TestAnthropicRequestShapeRetryAndParse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth/version headers")
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		format := body["output_config"].(map[string]any)["format"].(map[string]any)
		if format["type"] != "json_schema" {
			t.Errorf("output_config.format.type = %v", format["type"])
		}
		for _, bad := range []string{`"minimum"`, `"tool_choice"`, `"temperature"`} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("request body contains %s", bad)
			}
		}
		sys, _ := body["system"].([]any)
		if body["max_tokens"].(float64) != 8192 || len(sys) != 1 || sys[0].(map[string]any)["text"] != "sys" {
			t.Errorf("max_tokens=%v system=%v", body["max_tokens"], body["system"])
		}
		content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		img := content[1].(map[string]any)
		if img["type"] != "image" || img["source"].(map[string]any)["media_type"] != "image/jpeg" {
			t.Errorf("bad image block %v", img)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(529)
			return
		}
		w.Write([]byte(anthropicReply(`{"score": 7.5}`, "end_turn")))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "m")
	a.Endpoint = srv.URL
	resp, err := a.Call(context.Background(), tinyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(resp.JSON) != `{"score": 7.5}` || resp.Usage.InputTokens != 3000 {
		t.Fatalf("calls=%d json=%s usage=%+v", calls, resp.JSON, resp.Usage)
	}
}

func TestAnthropicStopReasonsAndFatalStatus(t *testing.T) {
	cases := []struct {
		status        int
		reply, errHas string
		wantCalls     int32
	}{
		{200, anthropicReply("", "refusal"), "refus", 1},
		{200, anthropicReply(`{"score":`, "max_tokens"), "max_tokens", 1},
		{400, `{"error":{"message":"bad"}}`, "400", 1},
	}
	for _, c := range cases {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(c.status)
			w.Write([]byte(c.reply))
		}))
		a := NewAnthropic("k", "m")
		a.Endpoint = srv.URL
		_, err := a.Call(context.Background(), tinyRequest())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.errHas) || calls != c.wantCalls {
			t.Errorf("status %d: calls=%d err=%v (want %q)", c.status, calls, err, c.errHas)
		}
	}
}

// The system prompt (with the output format) is identical for every frame of a run:
// cache it, so frames after the first read it at a tenth of the price.
func TestAnthropicCachesTheSystemPrompt(t *testing.T) {
	p := NewAnthropic("k", "claude-sonnet-5-5").params(tinyRequest())
	blocks, ok := p["system"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("system %#v", p["system"])
	}
	b := blocks[0].(map[string]any)
	cc, _ := b["cache_control"].(map[string]any)
	if b["type"] != "text" || b["text"] != "sys" || cc["type"] != "ephemeral" {
		t.Fatalf("block %#v", b)
	}
}

func TestParseAnthropicCacheUsage(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"content":     []any{map[string]any{"type": "text", "text": `{"score":1}`}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5000, "output_tokens": 100, "cache_creation_input_tokens": 900, "cache_read_input_tokens": 40},
	})
	r, err := parseAnthropic(body)
	if err != nil || r.Usage.CacheWriteTokens != 900 || r.Usage.CacheReadTokens != 40 || r.Usage.InputTokens != 5000 || r.Usage.TotalIn() != 5940 {
		t.Fatalf("usage %+v err %v", r.Usage, err)
	}
}
