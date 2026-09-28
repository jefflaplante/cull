package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeBatchServer implements create, retrieve and results for one batch.
type fakeBatchServer struct {
	mu       sync.Mutex
	creates  int
	polls    int
	requests []map[string]any
	lines    []string // results JSONL
}

func (f *fakeBatchServer) handler(t *testing.T, url *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth headers on %s", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/batches":
			f.creates++
			var body struct {
				Requests []map[string]any `json:"requests"`
			}
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body)
			f.requests = body.Requests
			fmt.Fprint(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress","request_counts":{"processing":2}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_1":
			f.polls++
			if f.polls < 2 {
				fmt.Fprint(w, `{"id":"msgbatch_1","processing_status":"in_progress","request_counts":{"processing":2}}`)
				return
			}
			fmt.Fprintf(w, `{"id":"msgbatch_1","processing_status":"ended","request_counts":{"succeeded":1,"errored":1},"results_url":"%s/v1/messages/batches/msgbatch_1/results"}`, *url)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_1/results":
			fmt.Fprint(w, strings.Join(f.lines, "\n")+"\n")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}
}

func TestBatchSubmitPollAndCollect(t *testing.T) {
	fs := &fakeBatchServer{lines: []string{ // results arrive in any order
		`{"custom_id":"b","result":{"type":"errored","error":{"type":"error","error":{"type":"overloaded_error","message":"busy"}}}}`,
		`{"custom_id":"a","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"{\"score\": 4}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}}}`,
	}}
	var url string
	srv := httptest.NewServer(fs.handler(t, &url))
	defer srv.Close()
	url = srv.URL

	a := NewAnthropic("k", "m")
	a.Endpoint = srv.URL + "/v1/messages"
	id, err := a.SubmitBatch(context.Background(), []BatchRequest{{CustomID: "a", Req: tinyRequest()}, {CustomID: "b", Req: tinyRequest()}})
	if err != nil || id != "msgbatch_1" || fs.creates != 1 {
		t.Fatalf("submit: id=%q creates=%d err=%v", id, fs.creates, err)
	}
	params := fs.requests[0]["params"].(map[string]any)
	if fs.requests[0]["custom_id"] != "a" || params["model"] != "m" || params["output_config"] == nil {
		t.Fatalf("request shape %v", fs.requests[0])
	}

	st, err := a.BatchStatus(context.Background(), id)
	if err != nil || st.Ended {
		t.Fatalf("first poll: %+v %v", st, err)
	}
	st, err = a.BatchStatus(context.Background(), id)
	if err != nil || !st.Ended || st.ResultsURL == "" {
		t.Fatalf("second poll: %+v %v", st, err)
	}
	got := map[string]BatchResult{}
	if err := a.BatchResults(context.Background(), st.ResultsURL, func(string) map[string]any { return tinySchema }, func(r BatchResult) { got[r.CustomID] = r }); err != nil {
		t.Fatal(err)
	}
	if got["a"].Err != nil || string(got["a"].Response.JSON) != `{"score": 4}` || got["a"].Response.Usage.InputTokens != 10 {
		t.Fatalf("a: %+v", got["a"])
	}
	if got["b"].Err == nil || !strings.Contains(got["b"].Err.Error(), "busy") {
		t.Fatalf("b: %+v", got["b"])
	}
}

func TestBatchResultFailingSchemaIsAnError(t *testing.T) {
	fs := &fakeBatchServer{lines: []string{
		`{"custom_id":"a","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"{\"nope\": 1}"}],"stop_reason":"end_turn","usage":{}}}}`,
		`{"custom_id":"c","result":{"type":"expired"}}`,
	}}
	var url string
	srv := httptest.NewServer(fs.handler(t, &url))
	defer srv.Close()
	url = srv.URL
	a := NewAnthropic("k", "m")
	a.Endpoint = srv.URL + "/v1/messages"
	got := map[string]BatchResult{}
	a.BatchResults(context.Background(), srv.URL+"/v1/messages/batches/msgbatch_1/results", func(string) map[string]any { return tinySchema }, func(r BatchResult) { got[r.CustomID] = r })
	if got["a"].Err == nil || !strings.Contains(got["a"].Err.Error(), "schema") || got["c"].Err == nil || !strings.Contains(got["c"].Err.Error(), "expired") {
		t.Fatalf("got %+v", got)
	}
}

func TestSubmitBatchRetriesOnlyWhenNothingWasCreated(t *testing.T) {
	for _, c := range []struct {
		statuses     []int
		wantCalls    int32
		wantRejected bool
		ok           bool
	}{
		{[]int{429, 200}, 2, false, true},  // rate limited: not created, safe to retry
		{[]int{500, 200}, 1, false, false}, // server error: the batch may exist, don't retry
		{[]int{400}, 1, true, false},       // rejected: definitely not created
	} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			w.Header().Set("retry-after", "0")
			if st := c.statuses[n-1]; st != 200 {
				w.WriteHeader(st)
				return
			}
			fmt.Fprint(w, `{"id":"msgbatch_9"}`)
		}))
		a := NewAnthropic("k", "m")
		a.Endpoint = srv.URL + "/v1/messages"
		id, err := a.SubmitBatch(context.Background(), []BatchRequest{{CustomID: "a", Req: tinyRequest()}})
		srv.Close()
		if calls != c.wantCalls || (err == nil) != c.ok || errors.Is(err, ErrRejected) != c.wantRejected || (c.ok && id != "msgbatch_9") {
			t.Errorf("statuses %v: calls=%d id=%q err=%v", c.statuses, calls, id, err)
		}
	}
}
