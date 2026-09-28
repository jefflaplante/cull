package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/llm"
)

type rankFake struct {
	answers []string
	reqs    []llm.Request
}

func (f *rankFake) Name() string { return "fake" }
func (f *rankFake) Call(_ context.Context, r llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, r)
	a := f.answers[0]
	f.answers = f.answers[1:]
	return &llm.Response{JSON: json.RawMessage(a), Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func frames3() []RankFrame {
	return []RankFrame{{Full: []byte{1}, Crop: []byte{2}}, {Full: []byte{3}}, {Full: []byte{4}, Crop: []byte{5}}}
}

const good3 = `{"ranking":[{"frame":2,"strength":"eyes","weakness":"tilt"},{"frame":1,"strength":"s","weakness":"w"},{"frame":3,"strength":"s","weakness":"w"}],"summary":"frame 2 has the moment"}`

func TestRankRequestHasNoNamesOrScores(t *testing.T) {
	req := RankRequest(frames3(), 2000)
	var jpegs int
	for _, p := range req.Parts {
		if p.JPEG != nil {
			jpegs++
		}
		for _, bad := range []string{".DNG", "score", "sharp:", "keep", "cull"} {
			if strings.Contains(p.Text, bad) {
				t.Errorf("part text leaks %q: %q", bad, p.Text)
			}
		}
	}
	if jpegs != 5 || !strings.Contains(req.Parts[0].Text+req.Parts[1].Text, "Frame 1") {
		t.Fatalf("5 images (3 full + 2 crops), labelled Frame 1..3; got %d", jpegs)
	}
	for _, want := range []string{"sharp", "expression", "moment", "composition", "fixed"} {
		if !strings.Contains(strings.ToLower(req.System), want) {
			t.Errorf("rubric lacks %q", want)
		}
	}
}

func TestRankDecodes(t *testing.T) {
	f := &rankFake{answers: []string{good3}}
	r, u, err := Rank(context.Background(), f, frames3(), 2000)
	if err != nil || r.Ranking[0].Frame != 2 || r.Summary == "" || u.InputTokens != 100 {
		t.Fatalf("%+v %+v %v", r, u, err)
	}
}

func TestRankRejectsNonPermutation(t *testing.T) {
	for name, bad := range map[string]string{
		"missing":  `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":2,"strength":"","weakness":""}],"summary":""}`,
		"repeated": `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":1,"strength":"","weakness":""},{"frame":3,"strength":"","weakness":""}],"summary":""}`,
		"range":    `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":2,"strength":"","weakness":""},{"frame":4,"strength":"","weakness":""}],"summary":""}`,
	} {
		f := &rankFake{answers: []string{bad, good3}}
		if r, _, err := Rank(context.Background(), f, frames3(), 2000); err != nil || r.Ranking[0].Frame != 2 || len(f.reqs) != 2 {
			t.Errorf("%s: one retry then success: %+v %v calls=%d", name, r, err, len(f.reqs))
		}
		f = &rankFake{answers: []string{bad, bad}}
		if _, _, err := Rank(context.Background(), f, frames3(), 2000); err == nil {
			t.Errorf("%s: twice bad must fail, never half-apply", name)
		}
	}
}
