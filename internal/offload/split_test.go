package offload

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// event lays out files named prefix+1.. at start, one per interval.
func event(files map[string]spec, first int, n int, start time.Time, step time.Duration) {
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("DCIM/100LEICA/M%04d.DNG", first+i)] = spec{seed: byte(first + i), mtime: start.Add(time.Duration(i) * step)}
	}
}

func folders(ps []*Plan) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Folder)
	}
	return out
}

func fileNames(p *Plan) string {
	var out []string
	for _, f := range p.Files {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

// A capture-time gap over SplitGap starts a new event; each event is its own
// numbered shoot folder holding only its frames.
func TestSplitAtTimeGaps(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 3, t0, time.Minute)                  // 14:00–14:02
	event(files, 4, 2, t0.Add(5*time.Hour), time.Minute) // 19:00–19:01
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	ps, err := MakePlans(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(folders(ps), " | "); got != "2026-10-02 Smith wedding 1 | 2026-10-02 Smith wedding 2" {
		t.Fatalf("folders %s", got)
	}
	if fileNames(ps[0]) != "M0001.DNG,M0002.DNG,M0003.DNG" || fileNames(ps[1]) != "M0004.DNG,M0005.DNG" {
		t.Fatalf("events %s / %s", fileNames(ps[0]), fileNames(ps[1]))
	}
	if ps[0].Dests[0] != filepath.Join(o.Dest, "2026-10-02 Smith wedding 1") {
		t.Fatalf("dest %s", ps[0].Dests[0])
	}
}

// A new day starts a new event even when the gap is short, and each event is dated
// by its own frames.
func TestSplitAtNewDay(t *testing.T) {
	src := t.TempDir()
	late := time.Date(2026, 10, 2, 23, 30, 0, 0, time.Local)
	files := map[string]spec{}
	event(files, 1, 2, late, time.Minute)
	event(files, 3, 2, late.Add(time.Hour), time.Minute) // 00:30 the next day
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	ps, err := MakePlans(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(folders(ps), " | "); got != "2026-10-02 Smith wedding 1 | 2026-10-03 Smith wedding 2" {
		t.Fatalf("folders %s", got)
	}
}

// One event found: the folder isn't numbered, as without --split.
func TestSplitWithOneEvent(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 4, t0, 10*time.Minute)
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	ps, err := MakePlans(o)
	if err != nil || len(ps) != 1 || ps[0].Folder != "2026-10-02 Smith wedding" {
		t.Fatalf("%v %v", folders(ps), err)
	}
}

// A clock that wasn't running stamps every frame alike: time can't place the split,
// so --split refuses and names the way that works.
func TestSplitRefusesBrokenClock(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 20, t0, 0)
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	if _, err := MakePlans(o); err == nil || !strings.Contains(err.Error(), "--split-at") {
		t.Fatalf("want a refusal naming --split-at, got %v", err)
	}
}

// --split-at starts an event at each named file (camera order), with or without its
// extension, whatever the clock says.
func TestSplitAtFileNames(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 6, t0, 0) // broken clock
	card(t, src, files)
	o := opts(t, src)
	o.SplitAt = []string{"M0003", "m0005.dng"}
	ps, err := MakePlans(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || fileNames(ps[0]) != "M0001.DNG,M0002.DNG" || fileNames(ps[1]) != "M0003.DNG,M0004.DNG" || fileNames(ps[2]) != "M0005.DNG,M0006.DNG" {
		t.Fatalf("%v", folders(ps))
	}
	o.SplitAt = []string{"M0099"}
	if _, err := MakePlans(o); err == nil || !strings.Contains(err.Error(), "M0099") {
		t.Fatalf("unknown split point: %v", err)
	}
	o.SplitAt, o.Split = []string{"M0003"}, true
	if _, err := MakePlans(o); err == nil {
		t.Fatal("--split with --split-at accepted")
	}
}

// Free space is checked for every event together: two that fit alone can still
// overfill the drive.
func TestSplitChecksSpaceForAllEvents(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 2, t0, time.Minute)
	event(files, 3, 2, t0.Add(5*time.Hour), time.Minute)
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	o.freeSpace = func(string) (uint64, error) { return reserve(3000), nil } // 4000 bytes to copy
	ps, err := MakePlans(o)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") || len(ps) != 2 {
		t.Fatalf("plans %d err %v", len(ps), err)
	}
}

// Each event copies into its own folder, with its own manifest, and verifies.
func TestSplitRunsEachEvent(t *testing.T) {
	src := t.TempDir()
	files := map[string]spec{}
	event(files, 1, 2, t0, time.Minute)
	event(files, 3, 3, t0.Add(5*time.Hour), time.Minute)
	card(t, src, files)
	o := opts(t, src)
	o.Split = true
	o.Backup = t.TempDir()
	ps, err := MakePlans(o)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range ps {
		if len(p.Dests) != 2 || filepath.Base(p.Dests[1]) != p.Folder {
			t.Fatalf("event %d dests %v", i+1, p.Dests)
		}
		res, err := Run(context.Background(), p, nil)
		if err != nil || !res.Safe || res.Copied != len(p.Files) {
			t.Fatalf("event %d: %+v %v", i+1, res, err)
		}
		v, err := Verify(context.Background(), p.Dests[0], nil)
		if err != nil || v.OK != len(p.Files) || v.Bad != 0 {
			t.Fatalf("event %d verify %+v %v", i+1, v, err)
		}
	}
	// A rerun maps the card onto the same folders and copies nothing.
	ps, err = MakePlans(o)
	if err != nil || len(ps) != 2 {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Bytes != 0 {
			t.Fatalf("%s would copy %d bytes again", p.Folder, p.Bytes)
		}
	}
}
