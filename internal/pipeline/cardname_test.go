package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/offload"
)

// A scan records each frame's card name (card folder and camera name) from the
// manifest, and a resumed result takes it too (a report from before card names), with
// no model call.
func TestCardNameFromManifest(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "a_0001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.DNG"))
	man, _ := os.Create(filepath.Join(dir, offload.ManifestName))
	json.NewEncoder(man).Encode(offload.Entry{Src: "/Volumes/LEICA M/DCIM/100LEICA/M1100001.DNG", Orig: "M1100001.DNG", Name: "a_0001.DNG", Size: 1})
	man.Close()
	rep, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rep.Results {
		got[filepath.Base(r.File)] = r.CardName
	}
	if !reflect.DeepEqual(got, map[string]string{"a_0001.DNG": "100LEICA/M1100001.DNG", "L1000002.DNG": ""}) {
		t.Fatalf("card names %v", got)
	}

	f, _ := os.OpenFile(filepath.Join(dir, offload.ManifestName), os.O_APPEND|os.O_WRONLY, 0o644)
	json.NewEncoder(f).Encode(offload.Entry{Src: "/Volumes/LEICA M/DCIM/100LEICA/L1000002.DNG", Orig: "L1000002.DNG", Name: "L1000002.DNG", Size: 1})
	f.Close()
	c := cfg(dir)
	c.Resume = true
	b := &fakeBackend{status: "sharp"}
	rep, _, err = Run(context.Background(), c, b)
	if err != nil || b.calls != 0 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
	for _, r := range rep.Results {
		if filepath.Base(r.File) == "L1000002.DNG" && r.CardName != "100LEICA/L1000002.DNG" {
			t.Fatalf("resumed card name %q", r.CardName)
		}
	}
}

// Sets are in camera order of the card names, whatever the frames are called now.
func TestSetsInCardNameOrder(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9) // /s/L001.DNG … L004
	for i, n := range []string{"100LEICA/L1002772.DNG", "100LEICA/M1102771.DNG", "100LEICA/L1002773.DNG", "100LEICA/M1102770.DNG"} {
		rep.Results[i].CardName = n
	}
	decideAll(rep, eval.Policy{KeepBest: 2, Outranked: eval.ActionReview}, seq)
	want := []string{"/s/L004.DNG", "/s/L002.DNG", "/s/L001.DNG", "/s/L003.DNG"}
	if len(rep.Sets) != 1 || !reflect.DeepEqual(rep.Sets[0].Members, want) {
		t.Fatalf("sets %+v", rep.Sets)
	}
}
