package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sseChunk(w http.ResponseWriter, content string, usage map[string]any) {
	c := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.(http.Flusher).Flush()
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	return body
}

func TestOpenAIStreamHangsUpWhenJSONCloses(t *testing.T) {
	disconnected := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		body := decodeBody(t, r)
		rf := body["response_format"].(map[string]any)
		js := rf["json_schema"].(map[string]any)
		if rf["type"] != "json_schema" || js["strict"] != true || strings.Contains(fmt.Sprint(js["schema"]), "minimum") {
			t.Errorf("response_format = %v", rf)
		}
		msgs := body["messages"].([]any)
		if msgs[0].(map[string]any)["role"] != "system" || body["temperature"].(float64) != 0 || body["stream"] != true {
			t.Errorf("messages[0]=%v temperature=%v stream=%v", msgs[0], body["temperature"], body["stream"])
		}
		img := msgs[1].(map[string]any)["content"].([]any)[1].(map[string]any)
		if url := img["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(url, "data:image/jpeg;base64,") {
			t.Errorf("image url %.40s", url)
		}
		w.Header().Set("content-type", "text/event-stream")
		sseChunk(w, `{"sco`, nil)
		sseChunk(w, `re": 7`, nil)
		sseChunk(w, `.5}`, map[string]any{"prompt_tokens": 441, "completion_tokens": 9})
		for i := 0; i < 200; i++ { // a server that keeps generating after the JSON
			select {
			case <-r.Context().Done():
				disconnected <- true
				return
			case <-time.After(10 * time.Millisecond):
				sseChunk(w, " ", nil)
			}
		}
		disconnected <- false
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL+"/v1", "k", "m")
	resp, err := o.Call(context.Background(), tinyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"score": 7.5}` || resp.Usage.InputTokens != 441 || resp.Usage.OutputTokens != 9 {
		t.Fatalf("json=%s usage=%+v", resp.JSON, resp.Usage)
	}
	select {
	case early := <-disconnected:
		if !early {
			t.Fatal("client kept reading until the server stopped; expected an early hang-up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed the disconnect")
	}
}

func TestOpenAINonStreamExtractsFencedJSONAndNoKeyNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header without a key")
		}
		body := decodeBody(t, r)
		if body["stream"] == true || body["max_tokens"].(float64) != 256 {
			t.Errorf("stream=%v max_tokens=%v", body["stream"], body["max_tokens"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "```json\n{\"score\": 3}\n```"}, "finish_reason": "length"}},
			"usage":   map[string]any{"prompt_tokens": 441, "completion_tokens": 300},
		})
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "", "m")
	o.Stream = false
	req := tinyRequest()
	req.MaxTokens = 256
	resp, err := o.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"score": 3}` || resp.Usage.InputTokens != 441 || resp.Usage.OutputTokens != 300 {
		t.Fatalf("json=%s usage=%+v", resp.JSON, resp.Usage)
	}
}

func TestOpenAIRetriesTransientButNotAuthErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(429)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"score":1}`}}}})
	}))
	o := NewOpenAI(srv.URL, "", "m")
	o.Stream = false
	if _, err := o.Call(context.Background(), tinyRequest()); err != nil || calls != 2 {
		t.Fatalf("429 then 200: calls=%d err=%v", calls, err)
	}
	srv.Close()

	calls = 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	o = NewOpenAI(srv.URL, "", "m")
	if _, err := o.Call(context.Background(), tinyRequest()); err == nil || !strings.Contains(err.Error(), "401") || calls != 1 {
		t.Fatalf("401: calls=%d err=%v", calls, err)
	}
}

func TestOpenAIRetriesIncompleteOutputOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			sseChunk(w, "{\n  \"score\":", nil) // e.g. the model looped until max_tokens
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		sseChunk(w, `{"score": 4}`, nil)
	}))
	defer srv.Close()
	resp, err := NewOpenAI(srv.URL, "", "m").Call(context.Background(), tinyRequest())
	if err != nil || calls != 2 || string(resp.JSON) != `{"score": 4}` {
		t.Fatalf("calls=%d err=%v resp=%v", calls, err, resp)
	}
}

func TestOpenAIIncompleteOutputErrorIsOneLineWithTail(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		sseChunk(w, "{\n  \"issues\": [\n    \"tilted\",\n    \"tilted again at the very end\"", nil)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	_, err := NewOpenAI(srv.URL, "", "m").Call(context.Background(), tinyRequest())
	if err == nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "incomplete JSON") || !strings.Contains(msg, "tilted again at the very end") || strings.ContainsAny(msg, "\n\r") {
		t.Fatalf("want a one-line error quoting the tail, got %q", msg)
	}
}

func TestOpenAINonJSONBodyFailsFast(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, "<html>proxy login</html>")
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "", "m")
	o.Stream = false
	if _, err := o.Call(context.Background(), tinyRequest()); err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
