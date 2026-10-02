package llm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDebugNeverPrintsKey(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(529)
			w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		w.Write([]byte(anthropicReply(`{"score": 7.5}`, "end_turn")))
	}))
	defer srv.Close()

	var dbg bytes.Buffer
	a := NewAnthropic("sk-test-SECRET", "claude-sonnet-5")
	a.Endpoint = srv.URL
	a.Debug = &dbg
	if _, err := a.Call(context.Background(), tinyRequest()); err != nil {
		t.Fatal(err)
	}
	out := dbg.String()
	for _, want := range []string{"POST", "bytes", "claude-sonnet-5", "529", "200", "1 image", "score"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("debug printed the API key:\n%s", out)
	}
}

func TestClaudeCodeDebugShowsInitAndQuota(t *testing.T) {
	bin, _ := setupFake(t, initEvent("none"), rateEvent("allowed", 0.2), okResult)
	var dbg bytes.Buffer
	c := NewClaudeCode(bin, "sonnet", 0.9)
	c.Debug = &dbg
	req := tinyRequest()
	req.System = strings.Repeat("long system prompt ", 50)
	if _, err := c.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	out := dbg.String()
	for _, want := range []string{"apiKeySource=none", "model=claude-sonnet-5", "5-hour 20%", "--system-prompt <", "score"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "long system prompt long") {
		t.Fatalf("debug printed the system prompt instead of its length:\n%s", out)
	}
}

func TestOpenAIDebugShowsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"score\": 4}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`))
	}))
	defer srv.Close()
	var dbg bytes.Buffer
	o := NewOpenAI(srv.URL, "local-SECRET", "m")
	o.Stream = false
	o.Debug = &dbg
	if _, err := o.Call(context.Background(), tinyRequest()); err != nil {
		t.Fatal(err)
	}
	out := dbg.String()
	if !strings.Contains(out, "POST") || !strings.Contains(out, "200") || strings.Contains(out, "SECRET") {
		t.Fatalf("openai debug:\n%s", out)
	}
}
