package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
)

func TestSchemaV4RoundTrip(t *testing.T) {
	look := make([]uint8, 192)
	look[5] = 200
	r := &Report{SchemaVersion: SchemaVersion, KeepBest: 3,
		Results: []Result{{File: "/s/L1.DNG", Look: EncodeLook(look), CostUSD: 0.02,
			Group: &Group{ID: 1, Size: 2, Rank: 1, Of: 2, By: "model", Best: true, Strength: "eyes", Weakness: "tilt"}}},
		Sets: []Set{{ID: 1, Members: []string{"/s/L1.DNG", "/s/L2.DNG"}, Of: 2, Order: []string{"/s/L1.DNG", "/s/L2.DNG"},
			Notes: []RankNote{{File: "/s/L1.DNG", Strength: "eyes", Weakness: "tilt"}}, Summary: "sharper eyes", By: "model",
			Usage: eval.Usage{InputTokens: 9000, OutputTokens: 800}, CostUSD: 0.03}}}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := r.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || SchemaVersion != 4 || got.KeepBest != 3 || len(got.Sets) != 1 || got.Sets[0].Summary != "sharper eyes" || got.Sets[0].Of != 2 {
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

// Schema v3 recorded group.best as the base name of the frame kept from a burst.
// Such reports must still load (decide, restore, review, apply-c1 and calibrate
// read them); the name becomes Best false, and decide regroups.
func TestLoadsV3GroupsWithStringBest(t *testing.T) {
	v3 := `{"schema_version": 3, "backend": "anthropic", "model": "m", "dir": "/s", "results": [
  {"file": "/s/L1.DNG", "size": 10, "dhash": "00ff00ff00ff00ff", "group": {"id": 1, "size": 2, "best": "L2.DNG"}},
  {"file": "/s/L2.DNG", "size": 11, "group": {"id": 1, "size": 2}}]}`
	p := filepath.Join(t.TempDir(), "v3.json")
	if err := os.WriteFile(p, []byte(v3), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("v3 report must load: %v", err)
	}
	if got.SchemaVersion != 3 || len(got.Results) != 2 {
		t.Fatalf("report: %+v", got)
	}
	for _, r := range got.Results {
		if g := r.Group; g == nil || *g != (Group{ID: 1, Size: 2}) {
			t.Fatalf("%s: group %+v", r.File, r.Group)
		}
	}

	got.Results[1].Group.Best = true
	if err := got.Save(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if s := string(raw); !strings.Contains(s, `"best": false`) || !strings.Contains(s, `"best": true`) || strings.Contains(s, `"best": "`) {
		t.Fatalf("saved groups must write best as a bool:\n%s", s)
	}
	again, err := Load(p)
	if err != nil || again.Results[0].Group.Best || !again.Results[1].Group.Best {
		t.Fatalf("round trip: %v %+v %+v", err, again.Results[0].Group, again.Results[1].Group)
	}

	var g Group
	if err := json.Unmarshal([]byte(`{"id": 1, "best": 3}`), &g); err == nil {
		t.Fatal("best that is neither a bool nor a string must still fail")
	}
}
