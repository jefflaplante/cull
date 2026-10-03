package pipeline

import (
	"context"
	"image"
	"image/color"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
)

func TestPrepareRecordsJunk(t *testing.T) {
	dir := t.TempDir()
	black := filepath.Join(dir, "L1.DNG")
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1067))
	for i := range img.Pix {
		img.Pix[i] = 4
	}
	dngWith(t, black, img)
	p, err := prepareFrame(cfg(dir), black)
	if err != nil {
		t.Fatal(err)
	}
	j := p.res.Junk
	if j == nil || j.Kind != "black" || j.DarkPct < 98 {
		t.Fatalf("junk %+v stats %+v", j, p.res.Stats)
	}

	real := filepath.Join(dir, "L2.DNG")
	g := image.NewRGBA(image.Rect(0, 0, 1600, 1067))
	for y := 0; y < 1067; y++ {
		for x := 0; x < 1600; x++ {
			v := uint8(x * 255 / 1600)
			g.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	dngWith(t, real, g)
	if p, _ := prepareFrame(cfg(dir), real); p.res.Junk != nil {
		t.Fatalf("a gradient flagged junk: %+v", p.res.Junk)
	}
}

// blackDNG writes a frame the junk filter flags as black.
func blackDNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1067))
	for i := range img.Pix {
		img.Pix[i] = 4
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	dngWith(t, path, img)
}

func junkShoot(t *testing.T) (string, Config) {
	t.Helper()
	dir := fourFiles(t)
	blackDNG(t, filepath.Join(dir, "L0000000.DNG"))
	c := cfg(dir)
	c.Policy.Junk = eval.ActionCull
	return dir, c
}

func TestJunkSkipsModel(t *testing.T) {
	_, c := junkShoot(t)
	b := &fakeBackend{status: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&b.calls); n != 4 {
		t.Fatalf("backend called %d times, want 4 (the junk frame skipped)", n)
	}
	r := result(t, rep, "L0000000.DNG")
	if r.Decision != eval.Cull || r.Evaluation != nil || len(r.Reasons) == 0 || !strings.HasPrefix(r.Reasons[0], "junk: black") {
		t.Fatalf("junk frame: decision %q evaluation %v reasons %v", r.Decision, r.Evaluation != nil, r.Reasons)
	}
}

func TestJunkIgnoreJudges(t *testing.T) {
	_, c := junkShoot(t)
	c.Policy.Junk = eval.ActionIgnore
	b := &fakeBackend{status: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&b.calls); n != 5 {
		t.Fatalf("backend called %d times, want 5", n)
	}
	if r := result(t, rep, "L0000000.DNG"); r.Junk == nil || r.Evaluation == nil {
		t.Fatalf("ignored junk frame: junk %v evaluated %v", r.Junk, r.Evaluation != nil)
	}
}

func TestResumeKeepsJunk(t *testing.T) {
	_, c := junkShoot(t)
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	b := &fakeBackend{status: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&b.calls); n != 0 {
		t.Fatalf("resume called the backend %d times", n)
	}
	if r := result(t, rep, "L0000000.DNG"); r.Decision != eval.Cull || len(rep.Results) != 5 {
		t.Fatalf("junk frame after resume: %q, %d results", r.Decision, len(rep.Results))
	}
}

func TestJunkOutsideSets(t *testing.T) {
	dir, c := junkShoot(t)
	blackDNG(t, filepath.Join(dir, "L0000001.DNG")) // two identical blank frames: a set, unless junk is kept out
	c.Seq = group.Options{Gap: time.Hour, MaxLook: group.DefaultLook}
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"L0000000.DNG", "L0000001.DNG"} {
		if r := result(t, rep, n); r.Group != nil {
			t.Fatalf("junk frame %s joined set %+v", n, r.Group)
		}
	}
	for _, s := range rep.Sets {
		for _, f := range s.Order {
			if filepath.Base(f) == "L0000000.DNG" {
				t.Fatal("junk frame in a set's order")
			}
		}
	}
}
