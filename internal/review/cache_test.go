package review

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/report"
)

var cachePV = &report.PreviewInfo{Width: 1600, Height: 1067, Orientation: 1, Source: "tiff-ifd"}

// cacheShoot is one frame with a focus box, as judge leaves it.
func cacheShoot(t *testing.T) (dir string, rep *report.Report) {
	t.Helper()
	dir = t.TempDir()
	f := filepath.Join(dir, "L1.DNG")
	tinyDNG(t, f)
	st, _ := os.Stat(f)
	rep = &report.Report{Dir: dir, Results: []report.Result{{
		File: f, Size: st.Size(), ModTime: st.ModTime(), Preview: cachePV,
		FocusTarget: &report.FocusTarget{Source: "face", Box: &eval.NormBox{Left: 0.4, Top: 0.3, Right: 0.6, Bottom: 0.5}},
	}}}
	return dir, rep
}

func assets(t *testing.T, out string) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(out, AssetsDir))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// An image's name carries what it was made from: the same frame and focus box give
// the same name; a changed file or a moved box gives a new one, so a stale image is
// never shown.
func TestAssetNamesFollowTheirInputs(t *testing.T) {
	_, rep := cacheShoot(t)
	r := rep.Results[0]
	thumb, subj := thumbName("L1", r), subjectName("L1", r, *r.FocusTarget.Box)
	if !strings.HasPrefix(thumb, "L1.thumb.") || !strings.HasPrefix(subj, "L1.subject.") || thumbName("L1", r) != thumb {
		t.Fatalf("names %s %s", thumb, subj)
	}
	changed := r
	changed.ModTime = r.ModTime.Add(time.Second)
	if thumbName("L1", changed) == thumb {
		t.Error("a changed file kept its thumbnail name")
	}
	moved := *r.FocusTarget.Box
	moved.Left += 0.05
	if subjectName("L1", r, moved) == subj {
		t.Error("a moved focus box kept its subject crop name")
	}
}

// A second build reuses every image: nothing is rendered again.
func TestBuildReusesCurrentImages(t *testing.T) {
	_, rep := cacheShoot(t)
	out := t.TempDir()
	if _, err := Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := assets(t, out)
	for _, n := range first {
		os.WriteFile(filepath.Join(out, AssetsDir, n), []byte("cached"), 0o644)
	}
	if _, err := Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, n := range assets(t, out) {
		if b, _ := os.ReadFile(filepath.Join(out, AssetsDir, n)); string(b) != "cached" {
			t.Errorf("%s was rendered again", n)
		}
	}
	if len(first) != 2 {
		t.Fatalf("want a thumbnail and a subject crop, got %v", first)
	}
}

// When a frame changes, its new images are rendered and the stale ones removed.
func TestBuildReplacesStaleImages(t *testing.T) {
	_, rep := cacheShoot(t)
	out := t.TempDir()
	Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard)
	old := assets(t, out)
	rep.Results[0].FocusTarget.Box.Left = 0.3 // re-judged: the focus box moved
	if _, err := Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard); err != nil {
		t.Fatal(err)
	}
	now := assets(t, out)
	if len(now) != 2 {
		t.Fatalf("assets %v", now)
	}
	gone := 0
	for _, n := range old {
		if !contains(now, n) {
			gone++
		}
	}
	if gone != 1 { // the subject crop changed; the thumbnail didn't
		t.Fatalf("old %v now %v", old, now)
	}
}

// Images from before names carried their inputs are adopted, not rendered again:
// upgrading cull mustn't re-render a big shoot.
func TestBuildAdoptsImagesFromBeforeCacheNames(t *testing.T) {
	_, rep := cacheShoot(t)
	out := t.TempDir()
	os.MkdirAll(filepath.Join(out, AssetsDir), 0o755)
	os.WriteFile(filepath.Join(out, AssetsDir, "L1.thumb.jpg"), []byte("old thumb"), 0o644)
	os.WriteFile(filepath.Join(out, AssetsDir, "L1.subject.jpg"), []byte("old subject"), 0o644)
	s, err := Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := s.data.Cards[0]
	for name, want := range map[string]string{c.Thumb: "old thumb", c.Subject: "old subject"} {
		if b, _ := os.ReadFile(filepath.Join(out, AssetsDir, name)); string(b) != want {
			t.Errorf("%s: %q, want the adopted %q", name, b, want)
		}
	}
	if got := assets(t, out); len(got) != 2 {
		t.Errorf("old names left behind: %v", got)
	}
}

// --force renders everything again, current or not.
func TestBuildForceRendersAgain(t *testing.T) {
	_, rep := cacheShoot(t)
	out := t.TempDir()
	Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard)
	for _, n := range assets(t, out) {
		os.WriteFile(filepath.Join(out, AssetsDir, n), []byte("cached"), 0o644)
	}
	Build(rep, "r.json", Options{Out: out, Concurrency: 1, Force: true}, io.Discard)
	for _, n := range assets(t, out) {
		if b, _ := os.ReadFile(filepath.Join(out, AssetsDir, n)); string(b) == "cached" {
			t.Errorf("%s not rendered again", n)
		}
	}
}

// Prerender, given the decoded preview judge already has, writes exactly the images
// Build would, so the next review renders nothing.
func TestPrerenderFillsTheCache(t *testing.T) {
	dir, rep := cacheShoot(t)
	out := t.TempDir()
	r := rep.Results[0]
	pv, err := dng.Extract(r.File)
	if err != nil {
		t.Fatal(err)
	}
	f, err := imageprep.Decode(pv.Data, pv.Orientation)
	if err != nil {
		t.Fatal(err)
	}
	if err := Prerender(out, dir, r, f); err != nil {
		t.Fatal(err)
	}
	pre := assets(t, out)
	for _, n := range pre {
		os.WriteFile(filepath.Join(out, AssetsDir, n), []byte("prerendered"), 0o644)
	}
	s, err := Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := s.data.Cards[0]
	for _, n := range []string{c.Thumb, c.Subject} {
		if b, _ := os.ReadFile(filepath.Join(out, AssetsDir, n)); string(b) != "prerendered" {
			t.Errorf("%s: review rendered it again (prerendered %v)", n, pre)
		}
	}
}

// ClearCache removes the images cull made and nothing else.
func TestClearCache(t *testing.T) {
	_, rep := cacheShoot(t)
	out := t.TempDir()
	Build(rep, "r.json", Options{Out: out, Concurrency: 1}, io.Discard)
	os.WriteFile(filepath.Join(out, AssetsDir, "notes.txt"), []byte("mine"), 0o644)
	n, err := ClearCache(out)
	if err != nil || n != 2 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if got := assets(t, out); len(got) != 1 || got[0] != "notes.txt" {
		t.Fatalf("left %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
