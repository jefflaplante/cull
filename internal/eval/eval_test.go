package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/llm"
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

func TestPolicyPeopleAndSharpnessThreshold(t *testing.T) {
	eyes := func(status, eyes, expr string, score float64) *Evaluation {
		return &Evaluation{
			Sharpness: Sharpness{Status: status, Score: score},
			Exposure:  Exposure{Status: "good"},
			People:    People{Present: true, Eyes: eyes, Expression: expr},
		}
	}
	cases := []struct {
		name   string
		p      Policy
		e      *Evaluation
		want   Decision
		reason string
	}{
		{"eyes closed default reviews", Policy{}, eyes("sharp", "closed", "good", 8), Review, "eyes closed"},
		{"eyes closed can cull", Policy{EyesClosed: ActionCull}, eyes("sharp", "closed", "good", 8), Cull, "eyes closed"},
		{"eyes closed can be ignored", Policy{EyesClosed: ActionIgnore}, eyes("sharp", "closed", "good", 8), Keep, ""},
		{"partial is a note", Policy{}, eyes("sharp", "partial", "good", 8), Keep, "partially closed"},
		{"awkward expression is a note", Policy{}, eyes("sharp", "open", "awkward", 8), Keep, "expression"},
		{"below sharpness threshold", Policy{ReviewBelowSharpness: 6}, eyes("acceptable", "open", "good", 5.5), Review, "below 6.0"},
		{"threshold off by default", Policy{}, eyes("acceptable", "open", "good", 2), Keep, ""},
		{"cull outranks review", Policy{}, eyes("missed_focus", "closed", "good", 2), Cull, "sharpness: missed_focus"},
		{"no person, no eye rules", Policy{}, &Evaluation{Sharpness: Sharpness{Status: "sharp", Score: 9}, People: People{Eyes: "not_visible"}}, Keep, ""},
	}
	for _, c := range cases {
		d, reasons := c.p.Decide(c.e)
		joined := strings.Join(reasons, "; ")
		if d != c.want || (c.reason != "" && !strings.Contains(joined, c.reason)) || (c.reason == "" && c.want == Keep && joined != "") {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, d, joined, c.want, c.reason)
		}
	}
}

func TestParseAction(t *testing.T) {
	for in, want := range map[string]Action{"ignore": ActionIgnore, "review": ActionReview, "cull": ActionCull} {
		if got, err := ParseAction(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := ParseAction("delete"); err == nil {
		t.Error("delete accepted")
	}
}

func TestEvaluationSchemaRequiresPeople(t *testing.T) {
	if err := llm.Validate(evaluationSchema, []byte(evalJSON)); err == nil || !strings.Contains(err.Error(), "people") {
		t.Fatalf("an evaluation without people must be rejected, got %v", err)
	}
}

func TestPolicyUsesRawClipping(t *testing.T) {
	clipped := func() *Evaluation {
		return &Evaluation{Sharpness: Sharpness{Status: "sharp", Score: 8}, Exposure: Exposure{Status: "clipped", Clipping: "highlights"}}
	}
	good := func() *Evaluation {
		return &Evaluation{Sharpness: Sharpness{Status: "sharp", Score: 8}, Exposure: Exposure{Status: "good"}}
	}
	cases := []struct {
		name   string
		p      Policy
		e      *Evaluation
		f      Facts
		want   Decision
		reason string
	}{
		{"no raw data: preview clipping reviews", Policy{}, clipped(), Facts{}, Review, "verify against raw"},
		{"raw has headroom: no review", Policy{}, clipped(), Facts{RawKnown: true, RawClipPct: 0.01}, Keep, "raw retains highlights"},
		{"raw clipped: review by default", Policy{}, clipped(), Facts{RawKnown: true, RawClipPct: 2}, Review, "raw highlights clipped: 2.00%"},
		{"raw clipped can cull", Policy{RawClipped: ActionCull}, clipped(), Facts{RawKnown: true, RawClipPct: 2}, Cull, "raw highlights clipped"},
		{"raw clipped though the model said good", Policy{}, good(), Facts{RawKnown: true, RawClipPct: 3}, Review, "raw highlights clipped"},
		{"custom threshold", Policy{RawClipThreshold: 5}, good(), Facts{RawKnown: true, RawClipPct: 3}, Keep, ""},
	}
	for _, c := range cases {
		d, reasons := c.p.DecideFacts(c.e, c.f)
		joined := strings.Join(reasons, "; ")
		if d != c.want || (c.reason != "" && !strings.Contains(joined, c.reason)) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, d, joined, c.want, c.reason)
		}
	}
}

func TestApplyOutranked(t *testing.T) {
	p := Policy{KeepBest: 3, Outranked: ActionReview}
	d, why := p.ApplyOutranked(Keep, nil, 5, 7, 3, false)
	if d != Review || len(why) != 1 || why[0] != "rank 5 of 7 in set 3 (keeping the best 3)" {
		t.Fatalf("%s %v", d, why)
	}
	if _, why := p.ApplyOutranked(Keep, nil, 4, 6, 2, true); why[0] != "rank 4 of 6 in set 2 by scores, not compared (keeping the best 3)" {
		t.Fatalf("%v", why)
	}
	if d, _ := (Policy{KeepBest: 3, Outranked: ActionCull}).ApplyOutranked(Review, nil, 5, 7, 3, false); d != Cull {
		t.Fatalf("cull action: %s", d)
	}
	if d, _ := (Policy{KeepBest: 3, Outranked: ActionIgnore}).ApplyOutranked(Keep, nil, 5, 7, 3, false); d != Keep {
		t.Fatalf("ignore: %s", d)
	}
}

func TestPromptWarnsAboutTextureComparisons(t *testing.T) {
	if p := SystemPrompt(0.6, Camera{}); !strings.Contains(p, "always look crisper than skin") {
		t.Fatal("prompt lacks the texture caveat")
	}
}
