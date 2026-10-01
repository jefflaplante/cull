package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
)

func TestSchemaV4RoundTrip(t *testing.T) {
	look := make([]uint8, 192)
	look[5] = 200
	r := &Report{SchemaVersion: SchemaVersion, KeepBest: 3, RankCostUSD: 0.04, // an earlier ranking of a set since dissolved: $0.01
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
	// Ranking spend counts from rank_cost_usd, which nothing drops; the sets' cost_usd
	// is per-set information and isn't added again.
	if c := got.Cost(); got.RankCostUSD != 0.04 || c < 0.0599 || c > 0.0601 {
		t.Fatalf("cost = results + rank_cost_usd: %v (rank %v)", c, got.RankCostUSD)
	}
}

// Schema v3 recorded group.best as the base name of the frame kept from a burst,
// and its group had no rank at all (decide/rank regroup from looks regardless,
// producing schema-v4 groups). Such reports must still load (decide, restore,
// review, apply-c1 and calibrate read them); Load drops the legacy group entirely
// so a card doesn't show a stale "set 0 · #0/0" badge before the first decide.
func TestLoadsV3DropsLegacyGroups(t *testing.T) {
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
		if r.Group != nil {
			t.Fatalf("%s: legacy group must be dropped on load, got %+v", r.File, r.Group)
		}
	}
}

// Group.UnmarshalJSON must still accept schema-v3's string "best" (a file name,
// decoded as false) even though Load now discards the result: it runs before Load
// gets a chance to nil the group out, and must not error the whole report load.
func TestGroupUnmarshalAcceptsLegacyStringBest(t *testing.T) {
	var g Group
	if err := json.Unmarshal([]byte(`{"id": 1, "size": 2, "best": "L2.DNG"}`), &g); err != nil {
		t.Fatalf("legacy string best: %v", err)
	}
	if g.Best {
		t.Fatalf("legacy string best must decode as false: %+v", g)
	}
	if err := json.Unmarshal([]byte(`{"id": 1, "best": true}`), &g); err != nil || !g.Best {
		t.Fatalf("bool best: %v %+v", err, g)
	}
	if err := json.Unmarshal([]byte(`{"id": 1, "best": 3}`), &g); err == nil {
		t.Fatal("best that is neither a bool nor a string must still fail")
	}
}

func TestLoadRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(p, []byte(fmt.Sprintf(`{"schema_version":%d,"results":[]}`, SchemaVersion+1)), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("got %v", err)
	}
}

func TestRebaseMovesEveryPathUnderTheOldDir(t *testing.T) {
	r := &Report{Dir: "/old", Results: []Result{{File: "/old/a/L1.DNG", MovedTo: "/old/a/culled/L1.DNG", XMP: "/old/a/culled/L1.xmp"}, {File: "/elsewhere/L2.DNG"}},
		Sets: []Set{{Members: []string{"/old/a/L1.DNG"}, Order: []string{"/old/a/L1.DNG"}, Notes: []RankNote{{File: "/old/a/L1.DNG"}}}}}
	if !r.Rebase("/new") {
		t.Fatal("no change reported")
	}
	x := r.Results[0]
	if r.Dir != "/new" || x.File != "/new/a/L1.DNG" || x.MovedTo != "/new/a/culled/L1.DNG" || x.XMP != "/new/a/culled/L1.xmp" ||
		r.Results[1].File != "/elsewhere/L2.DNG" || r.Sets[0].Members[0] != "/new/a/L1.DNG" || r.Sets[0].Order[0] != "/new/a/L1.DNG" || r.Sets[0].Notes[0].File != "/new/a/L1.DNG" {
		t.Fatalf("%+v %+v", r.Results, r.Sets)
	}
	if r.Rebase("/new") {
		t.Fatal("rebasing to the same dir changed something")
	}
	sib := &Report{Dir: "/old/shoot", Results: []Result{{File: "/old/shoot2/L1.DNG"}}}
	sib.Rebase("/new")
	if sib.Results[0].File != "/old/shoot2/L1.DNG" {
		t.Fatalf("a sibling folder sharing the prefix was rebased: %s", sib.Results[0].File)
	}
}

// A report run against another existing folder (a mistyped dir with -o) is not
// rebased: its frames still live where it says.
func TestRelocateOnlyWhenTheFolderMoved(t *testing.T) {
	parent := t.TempDir()
	a, b, reports := filepath.Join(parent, "a"), filepath.Join(parent, "b"), filepath.Join(parent, "reports")
	for _, d := range []string{a, b, reports} {
		os.Mkdir(d, 0o755)
	}
	mk := func() *Report { return &Report{Dir: a, Results: []Result{{File: filepath.Join(a, "L1.DNG")}}} }

	if r := mk(); r.Relocate(filepath.Join(reports, "a.json"), b) || r.Results[0].File != filepath.Join(a, "L1.DNG") {
		t.Fatalf("rebased onto a different existing folder: %+v", r.Results)
	}
	if r := mk(); !r.Relocate(filepath.Join(b, "cull-report.json"), b) { // a copy of the folder, its report inside
		t.Fatal("a report inside the folder it is used with must follow it")
	}
	link := filepath.Join(parent, "alias")
	os.Symlink(a, link)
	if r := mk(); r.Relocate(filepath.Join(reports, "a.json"), link) && r.Results[0].File != filepath.Join(link, "L1.DNG") {
		t.Fatal("same folder under another name: rebase or leave, but never mix")
	}
	gone := &Report{Dir: filepath.Join(parent, "gone"), Results: []Result{{File: filepath.Join(parent, "gone", "L1.DNG")}}}
	if !gone.Relocate(filepath.Join(reports, "a.json"), b) {
		t.Fatal("a folder that no longer exists was renamed: rebase")
	}
	none := &Report{Dir: filepath.Join(parent, "gone2"), Results: []Result{{File: "/elsewhere/L1.DNG"}}}
	if none.Relocate(filepath.Join(reports, "x.json"), b) || none.Dir != filepath.Join(parent, "gone2") {
		t.Fatal("nothing under the old folder: nothing to change, Dir kept")
	}
}
