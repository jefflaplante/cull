package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/offload"
)

// A scan records, for each frame the shoot folder's offload manifest says had its
// dates set, that date (Result.DatesSet), and the frame's sidecar carries it.
func TestScanRecordsDatesSetFromManifest(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.DNG"))
	man, _ := os.Create(filepath.Join(dir, offload.ManifestName))
	enc := json.NewEncoder(man)
	enc.Encode(offload.Entry{Orig: "L1000001.DNG", Name: "L1000001.DNG", Size: 1, DatesSet: "2026-10-04T12:00:00"})
	enc.Encode(offload.Entry{Orig: "L1000002.DNG", Name: "L1000002.DNG", Size: 1})
	man.Close()

	rep, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rep.Results {
		got[filepath.Base(r.File)] = r.DatesSet
		if filepath.Base(r.File) == "L1000001.DNG" {
			x, _ := os.ReadFile(r.XMP)
			if !strings.Contains(string(x), `exif:DateTimeOriginal="2026-10-04T12:00:00"`) {
				t.Errorf("sidecar lacks the set date:\n%s", x)
			}
		}
	}
	if got["L1000001.DNG"] != "2026-10-04T12:00:00" || got["L1000002.DNG"] != "" {
		t.Fatalf("DatesSet %v", got)
	}
	raw, _ := os.ReadFile(cfg(dir).ReportPath)
	if !strings.Contains(string(raw), `"dates_set": "2026-10-04T12:00:00"`) {
		t.Fatal("report lacks dates_set")
	}
}

// A resumed result takes the date the manifest records now, over the one the report
// held (redate appends a superseding manifest line), and makes no model call.
func TestResumeTakesManifestDatesSet(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	write := func(d string) {
		f, _ := os.OpenFile(filepath.Join(dir, offload.ManifestName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		json.NewEncoder(f).Encode(offload.Entry{Orig: "L1000001.DNG", Name: "L1000001.DNG", Size: 1, DatesSet: d})
		f.Close()
	}
	write("2026-10-04T12:00:00")
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	write("2026-10-05T09:00:00")
	c := cfg(dir)
	c.Resume = true
	b := &fakeBackend{status: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil || b.calls != 0 || len(rep.Results) != 1 || rep.Results[0].DatesSet != "2026-10-05T09:00:00" {
		t.Fatalf("err %v calls %d results %+v", err, b.calls, rep.Results)
	}
}
