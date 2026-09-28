package report

import (
	"path/filepath"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
)

func TestSchemaV4RoundTrip(t *testing.T) {
	look := make([]uint8, 192)
	look[5] = 200
	r := &Report{SchemaVersion: SchemaVersion, KeepBest: 3,
		Results: []Result{{File: "/s/L1.DNG", Look: EncodeLook(look), CostUSD: 0.02,
			Group: &Group{ID: 1, Size: 2, Rank: 1, Of: 2, By: "model", Best: true, Strength: "eyes", Weakness: "tilt"}}},
		Sets: []Set{{ID: 1, Members: []string{"/s/L1.DNG", "/s/L2.DNG"}, Order: []string{"/s/L1.DNG", "/s/L2.DNG"},
			Notes: []RankNote{{File: "/s/L1.DNG", Strength: "eyes", Weakness: "tilt"}}, Summary: "sharper eyes", By: "model",
			Usage: eval.Usage{InputTokens: 9000, OutputTokens: 800}, CostUSD: 0.03}}}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := r.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || SchemaVersion != 4 || got.KeepBest != 3 || len(got.Sets) != 1 || got.Sets[0].Summary != "sharper eyes" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if b, ok := got.Results[0].LookBytes(); !ok || len(b) != 192 || b[5] != 200 {
		t.Fatalf("look: %v %v", b, ok)
	}
	if !got.Results[0].Group.Best || got.Results[0].Group.By != "model" {
		t.Fatalf("group: %+v", got.Results[0].Group)
	}
	if c := got.Cost(); c < 0.0499 || c > 0.0501 {
		t.Fatalf("cost includes sets: %v", c)
	}
}
