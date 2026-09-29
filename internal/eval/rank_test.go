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
	// Sequences span up to a minute per link and different poses, not the same
	// instant: calling them "near-duplicates" invites the model to under-weight
	// genuine moment/gesture differences (rubric items 2-3).
	if strings.Contains(strings.ToLower(req.System), "near-duplicate") {
		t.Errorf("prompt must not call sequence members near-duplicates: %q", req.System)
	}
}

// The subject crops are cut by cull, not framed by the photographer. On the first live
// run the model listed "slightly tighter crop on face in the detail view" as a
// frame's weakness: it must judge framing on the full frame only.
func TestRankPromptSaysTheCropsAreTheTools(t *testing.T) {
	req := RankRequest(frames3(), 2000)
	sys := strings.ToLower(req.System)
	for _, want := range []string{"cut by this tool", "not by the photographer", "judge framing and composition on the full frame only"} {
		if !strings.Contains(sys, want) {
			t.Errorf("prompt lacks %q:\n%s", want, req.System)
		}
	}
	var crop string
	for _, p := range req.Parts {
		if strings.HasPrefix(p.Text, "Frame 1:") && !strings.Contains(p.Text, "full frame") {
			crop = p.Text
		}
	}
	if !strings.Contains(crop, "detail crop") || !strings.Contains(crop, "not the photo's framing") {
		t.Errorf("crop label should say it's a detail crop, not the framing: %q", crop)
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
