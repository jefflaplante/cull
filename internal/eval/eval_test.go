package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/llm"
)

const evalJSON = `{
 "sharpness":{"score":8.5,"status":"sharp","focus_target":"near eye"},
 "exposure":{"score":7,"status":"fixable","ev_adjust":0.7,"clipping":"none","reason":"under"},
 "composition":{"score":6,"status":"croppable","issues":["dead space left"],
   "crop":{"apply":true,"left":0.12,"top":0,"right":1,"bottom":0.95},"straighten_degrees":0},
 "notes":""}`

// fakeBackend records requests and replies with canned JSON per schema name.
type fakeBackend struct {
	replies map[string]string
	reqs    []llm.Request
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, req)
	return &llm.Response{JSON: json.RawMessage(f.replies[req.SchemaName]), Usage: llm.Usage{InputTokens: 3000, OutputTokens: 200}}, nil
}

func TestEvaluateSendsLabelledPartsInOrder(t *testing.T) {
	fb := &fakeBackend{replies: map[string]string{"evaluation": evalJSON}}
	in := Input{
		Filename:    "a.dng",
		FullFrame:   []byte{1},
		Subject:     &Labeled{Label: "SUBJECT", JPEG: []byte{2}},
		Landed:      []Labeled{{Label: "LANDED", JPEG: []byte{3}}},
		StatsText:   "STATS",
		MinCropArea: 0.6,
	}
	e, u, err := Evaluate(context.Background(), fb, in)
	if err != nil {
		t.Fatal(err)
	}
	if e.Sharpness.Status != "sharp" || u.InputTokens != 3000 || !e.Composition.Crop.Apply {
		t.Fatalf("eval=%+v usage=%+v", e, u)
	}
	req := fb.reqs[0]
	if req.SchemaName != "evaluation" || req.System == "" {
		t.Fatalf("schema=%q system empty=%v", req.SchemaName, req.System == "")
	}
	var seq []string
	for _, p := range req.Parts {
		switch {
		case p.JPEG != nil:
			seq = append(seq, fmt.Sprintf("img%d", p.JPEG[0]))
		case strings.Contains(p.Text, "SUBJECT"):
			seq = append(seq, "subject-label")
		case strings.Contains(p.Text, "LANDED"):
			seq = append(seq, "landed-label")
		case strings.Contains(p.Text, "STATS"):
			seq = append(seq, "stats")
		default:
			seq = append(seq, "text")
		}
	}
	want := "text img1 subject-label img2 landed-label img3 stats"
	if got := strings.Join(seq, " "); got != want {
		t.Fatalf("parts order\n got %s\nwant %s", got, want)
	}
}

func TestEvaluateWithoutSubjectSaysSo(t *testing.T) {
	fb := &fakeBackend{replies: map[string]string{"evaluation": evalJSON}}
	if _, _, err := Evaluate(context.Background(), fb, Input{Filename: "a.dng", FullFrame: []byte{1}, StatsText: "S"}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, p := range fb.reqs[0].Parts {
		text.WriteString(p.Text)
	}
	if !strings.Contains(strings.ToLower(text.String()), "no subject crop") {
		t.Fatalf("request does not tell the model the subject crop is missing: %q", text.String())
	}
}

func TestEvaluationSchemaClosesEveryObject(t *testing.T) {
	var walk func(any, string)
	walk = func(v any, path string) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		if m["type"] == "object" && m["additionalProperties"] != false {
			t.Errorf("%s: object without additionalProperties:false", path)
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for k, sub := range props {
				walk(sub, path+"."+k)
			}
		}
		walk(m["items"], path+"[]")
	}
	walk(evaluationSchema, "evaluation")
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
