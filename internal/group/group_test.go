package group

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// picture is a w×h luma plane of random 16px blocks (a "scene"), shifted and
// brightened slightly to mimic the next frame of a burst.
func picture(seed int64, w, h, shift int, gain float64) []uint8 {
	rng := rand.New(rand.NewSource(seed))
	blocks := make([]uint8, (w/16+2)*(h/16+2))
	for i := range blocks {
		blocks[i] = uint8(rng.Intn(256))
	}
	g := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := float64(blocks[(y/16)*(w/16+2)+(x+shift)/16]) * gain
			g[y*w+x] = uint8(min(v, 255))
		}
	}
	return g
}

func TestDHashSimilarVsDifferent(t *testing.T) {
	a := DHash(picture(1, 640, 480, 0, 1), 640, 480)
	b := DHash(picture(1, 640, 480, 3, 1.05), 640, 480) // same scene, next frame
	c := DHash(picture(2, 640, 480, 0, 1), 640, 480)    // another scene
	if a != DHash(picture(1, 640, 480, 0, 1), 640, 480) {
		t.Fatal("hash not deterministic")
	}
	if d := Hamming(a, b); d > 8 {
		t.Fatalf("burst neighbours too far apart: %d", d)
	}
	if d := Hamming(a, c); d < 20 {
		t.Fatalf("different scenes too close: %d", d)
	}
}

func TestGroupsByTimeAndHash(t *testing.T) {
	t0 := time.Date(2025, 12, 28, 0, 5, 59, 0, time.UTC)
	h, far := uint64(0xF0F0F0F0F0F0F0F0), uint64(0x0F0F0F0F0F0F0F0F)
	frames := []Frame{
		{Key: "a", Time: t0, HasTime: true, Hash: h, HasHash: true},
		{Key: "b", Time: t0, HasTime: true, Hash: h ^ 0x3, HasHash: true},                   // same second: 2 bits off
		{Key: "c", Time: t0.Add(time.Second), HasTime: true, Hash: h ^ 0x7, HasHash: true},  // 1 s later
		{Key: "d", Time: t0.Add(10 * time.Second), HasTime: true, Hash: h, HasHash: true},   // similar but 9 s gap
		{Key: "e", Time: t0.Add(11 * time.Second), HasTime: true, Hash: far, HasHash: true}, // close in time, different scene
		{Key: "f", Hash: far, HasHash: true},                                                // no time: grouped by name order + hash
		{Key: "g", Hash: far ^ 0x1, HasHash: true},
	}
	got := Groups(frames, Options{Gap: 2 * time.Second, MaxHamming: 12})
	want := [][]string{{"a", "b", "c"}, {"f", "g"}}
	if len(got) != len(want) {
		t.Fatalf("groups %v", keys(frames, got))
	}
	for i := range want {
		if g := keys(frames, got)[i]; len(g) != len(want[i]) || g[0] != want[i][0] || g[len(g)-1] != want[i][len(want[i])-1] {
			t.Fatalf("groups %v, want %v", keys(frames, got), want)
		}
	}
	if len(Groups(frames, Options{Gap: 0, MaxHamming: 12})) != 0 {
		t.Fatal("a zero gap must disable grouping")
	}
}

func keys(frames []Frame, groups [][]int) [][]string {
	var out [][]string
	for _, g := range groups {
		var ks []string
		for _, i := range g {
			ks = append(ks, frames[i].Key)
		}
		out = append(out, ks)
	}
	return out
}

func TestBestPrefersSharpThenEyesThenComposition(t *testing.T) {
	frames := []Frame{
		{Key: "a", Score: Score{Evaluated: true, Sharp: 7, EyesOpen: true, Comp: 9}},
		{Key: "b", Score: Score{Evaluated: true, Sharp: 8, EyesOpen: false, Comp: 5}},
		{Key: "c", Score: Score{Evaluated: true, Sharp: 8, EyesOpen: true, Comp: 4}},
		{Key: "d", Score: Score{Evaluated: false}},
	}
	if b := Best(frames, []int{0, 1, 2, 3}); frames[b].Key != "c" {
		t.Fatalf("best = %s, want c (sharpest with open eyes)", frames[b].Key)
	}
	if b := Best(frames, []int{3, 0}); frames[b].Key != "a" {
		t.Fatalf("an evaluated frame beats an unevaluated one, got %s", frames[b].Key)
	}
}

func TestGroupsOfNothing(t *testing.T) {
	if g := Groups(nil, Options{Gap: time.Second, MaxHamming: 12}); g != nil {
		t.Fatalf("got %v", g)
	}
	if g := Groups([]Frame{{Key: "a", HasHash: true}}, Options{Gap: time.Second, MaxHamming: 12}); g != nil {
		t.Fatalf("one frame is not a burst: %v", g)
	}
}

func TestGroupsDoNotChainAcrossASession(t *testing.T) {
	t0 := time.Date(2025, 12, 28, 0, 5, 59, 0, time.UTC)
	o := Options{Gap: 2 * time.Second, MaxHamming: 12}
	// Timestamps all equal (as on the sample's rewritten files); each frame differs
	// from its neighbour by 3 bits, so the hash drifts far over the session.
	var drift []Frame
	var h uint64
	for i := 0; i < 40; i++ {
		drift = append(drift, Frame{Key: fmt.Sprintf("M%04d", i), Time: t0, HasTime: true, Hash: h, HasHash: true})
		h ^= 0x7 << (3 * (i % 21))
	}
	for _, g := range Groups(drift, o) {
		first := drift[g[0]].Hash
		for _, i := range g {
			if Hamming(first, drift[i].Hash) > o.MaxHamming {
				t.Fatalf("burst chained to a frame %d bits from its first frame", Hamming(first, drift[i].Hash))
			}
		}
	}
	// Forty identical frames still split into bursts of at most MaxBurst.
	var same []Frame
	for i := 0; i < 40; i++ {
		same = append(same, Frame{Key: fmt.Sprintf("S%04d", i), Time: t0, HasTime: true, Hash: 42, HasHash: true})
	}
	for _, g := range Groups(same, o) {
		if len(g) > MaxBurst {
			t.Fatalf("burst of %d exceeds %d", len(g), MaxBurst)
		}
	}
}

// scene renders a synthetic 320×240 "photo": a background gradient with a subject
// block. dx/dy shift the whole view, gain scales brightness, zoom scales about
// the centre.
func scene(bg, subj [3]float64, sx, sy float64, dx, dy int, gain, zoom float64) []uint8 {
	const w, h = 320, 240
	grid := make([]uint8, LookSize*LookSize*3)
	for cy := 0; cy < LookSize; cy++ {
		for cx := 0; cx < LookSize; cx++ {
			var acc [3]float64
			n := 0
			for y := cy * h / LookSize; y < (cy+1)*h/LookSize; y += 4 {
				for x := cx * w / LookSize; x < (cx+1)*w/LookSize; x += 4 {
					u := (float64(x-w/2)/zoom + float64(w/2) + float64(dx)) / w
					v := (float64(y-h/2)/zoom + float64(h/2) + float64(dy)) / h
					c := bg
					for k := range c {
						c[k] = bg[k] * (0.6 + 0.4*v)
					}
					if math.Abs(u-sx) < 0.12 && math.Abs(v-sy) < 0.2 {
						c = subj
					}
					for k := range acc {
						acc[k] += math.Min(255, c[k]*gain)
					}
					n++
				}
			}
			for k := range acc {
				grid[(cy*LookSize+cx)*3+k] = uint8(acc[k] / float64(n))
			}
		}
	}
	return grid
}

var (
	park  = [3]float64{70, 140, 60}
	coat  = [3]float64{200, 60, 40}
	wall  = [3]float64{180, 170, 150}
	shirt = [3]float64{40, 60, 160}
)

func TestLookDistanceToleratesReframingAndExposure(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"shift 10%":     scene(park, coat, 0.5, 0.5, 32, 0, 1, 1),
		"one stop up":   scene(park, coat, 0.5, 0.5, 0, 0, 1.6, 1),
		"zoom 5%":       scene(park, coat, 0.5, 0.5, 0, 0, 1, 1.05),
		"subject moved": scene(park, coat, 0.56, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d > 0.08 {
			t.Errorf("%s: distance %.3f, want ≤ 0.08", name, d)
		}
	}
}

func TestLookDistanceSeparatesScenes(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"different scene":               scene(wall, shirt, 0.3, 0.6, 0, 0, 1, 1),
		"same place, different subject": scene(park, shirt, 0.3, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d < 0.12 {
			t.Errorf("%s: distance %.3f, want ≥ 0.12", name, d)
		}
	}
	if d := LookDistance(base, base); d != 0 {
		t.Errorf("identical: %.3f", d)
	}
}
