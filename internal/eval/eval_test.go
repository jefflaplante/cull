package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const toolResp = `{"content":[{"type":"tool_use","name":"record_evaluation","input":{
 "sharpness":{"score":8.5,"status":"sharp","focus_target":"near eye"},
 "exposure":{"score":7,"status":"fixable","ev_adjust":0.7,"clipping":"none","reason":"under"},
 "composition":{"score":6,"status":"croppable","issues":["dead space left"],
   "crop":{"apply":true,"left":0.12,"top":0,"right":1,"bottom":0.95},"straighten_degrees":0},
 "notes":""}}],"stop_reason":"tool_use","usage":{"input_tokens":3000,"output_tokens":200}}`

func TestEvaluateRetriesThenParses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing headers")
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		if tc := body["tool_choice"].(map[string]any); tc["name"] != toolName {
			t.Errorf("tool_choice not forced")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(529)
			return
		}
		w.Write([]byte(toolResp))
	}))
	defer srv.Close()

	c := NewClient("k", "m")
	c.Endpoint = srv.URL
	e, u, err := c.Evaluate(context.Background(), Input{Filename: "a.dng", FullFrame: []byte{0xFF, 0xD8}, MinCropArea: 0.6})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || e.Sharpness.Status != "sharp" || u.InputTokens != 3000 || !e.Composition.Crop.Apply {
		t.Fatalf("calls=%d eval=%+v usage=%+v", calls, e, u)
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{MinCropArea: 0.6}
	cases := []struct {
		sharp, exp string
		want       Decision
	}{
		{"sharp", "fixable", Keep},
		{"acceptable", "good", Keep},
		{"soft", "good", Review},
		{"missed_focus", "good", Cull},
		{"motion_blur", "good", Cull},
		{"sharp", "clipped", Review},
	}
	for _, c := range cases {
		e := &Evaluation{Sharpness: Sharpness{Status: c.sharp}, Exposure: Exposure{Status: c.exp}}
		if d, _ := p.Decide(e); d != c.want {
			t.Errorf("%s/%s: got %s want %s", c.sharp, c.exp, d, c.want)
		}
	}
}

func TestSanitizeDropsSmallCrop(t *testing.T) {
	e := &Evaluation{Composition: Composition{Crop: Crop{Apply: true, Left: 0.3, Top: 0.3, Right: 0.9, Bottom: 0.9}}}
	notes := Policy{MinCropArea: 0.6}.Sanitize(e)
	if e.Composition.Crop.Apply || len(notes) != 1 {
		t.Fatalf("crop not dropped: %+v %v", e.Composition.Crop, notes)
	}
}
