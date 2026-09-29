package eval

import (
	"context"
	"math"
	"testing"
)

func TestNormBoxValid(t *testing.T) {
	cases := []struct {
		b    NormBox
		want bool
	}{
		{NormBox{0.1, 0.2, 0.3, 0.4}, true},
		{NormBox{0, 0, 1, 1}, true},
		{NormBox{0.5, 0.2, 0.4, 0.4}, false}, // right < left
		{NormBox{0.1, 0.5, 0.3, 0.5}, false}, // zero height
		{NormBox{0.1, 0.2, 1.2, 0.4}, false}, // beyond the frame
		{NormBox{-0.1, 0.2, 0.3, 0.4}, false},
		{NormBox{math.NaN(), 0.2, 0.3, 0.4}, false},
		{NormBox{0.1, 0.2, math.Inf(1), 0.4}, false},
	}
	for _, c := range cases {
		if got := c.b.Valid(); got != c.want {
			t.Errorf("%+v.Valid() = %v, want %v", c.b, got, c.want)
		}
	}
}

func TestLocateSendsOneImageAndParses(t *testing.T) {
	fb := &fakeBackend{replies: map[string]string{
		"focus_target": `{"confident":true,"kind":"eye","subject":"woman's left eye","box":{"left":0.4,"top":0.2,"right":0.45,"bottom":0.25}}`,
	}}
	loc, u, err := Locate(context.Background(), fb, []byte{9}, Camera{}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !loc.Confident || loc.Kind != "eye" || loc.Box != (NormBox{0.4, 0.2, 0.45, 0.25}) || u.InputTokens != 3000 {
		t.Fatalf("loc=%+v usage=%+v", loc, u)
	}
	req := fb.reqs[0]
	images := 0
	for _, p := range req.Parts {
		if p.JPEG != nil {
			images++
		}
	}
	if req.SchemaName != "focus_target" || images != 1 || req.MaxTokens != 256 || req.System == "" {
		t.Fatalf("schema=%q images=%d max_tokens=%d", req.SchemaName, images, req.MaxTokens)
	}
}
