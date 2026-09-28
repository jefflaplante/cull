// Package group finds sequences of similar frames: consecutive frames close in
// capture time that look alike (LookDistance), such as a burst or several takes of
// one set-up.
package group

import (
	"math"
	"sort"
	"time"
)

// Score ranks frames within a set.
type Score struct {
	Evaluated bool
	Sharp     float64
	EyesOpen  bool
	Comp, Exp float64
}

// Frame is one image as grouping sees it.
type Frame struct {
	Key     string // file path; orders frames with equal or missing times
	Time    time.Time
	HasTime bool
	Look    []uint8 // look fingerprint (imageprep Grid(LookSize)); nil = never links
	Score   Score
}

// MaxSequence caps a set, so a slow walk down a street can't chain into one.
const MaxSequence = 40

// DefaultLook is the default Options.MaxLook, measured on 17 real frames
// (2026-09-28, CLAUDE.md): the two known pairs sit at 0.022 and 0.034, the nearest
// other consecutive pair at 0.118.
const DefaultLook = 0.08

// Options: Gap 0 disables grouping.
type Options struct {
	Gap     time.Duration // max capture-time gap to the previous frame (ignored when either lacks a time)
	MaxLook float64       // max LookDistance to the previous frame
}

// Sequences returns sets of two or more similar frames as indices into frames,
// each in capture order. Frames are ordered by capture time then key when every
// frame has a time, otherwise by key (Leica numbers are sequential). A frame joins
// the current set when it is within Gap of the previous frame and looks like the
// previous frame (not the first: sequences drift), up to MaxSequence frames.
func Sequences(frames []Frame, o Options) [][]int {
	if o.Gap <= 0 || len(frames) < 2 {
		return nil
	}
	allTimed := true
	for _, f := range frames {
		allTimed = allTimed && f.HasTime
	}
	order := make([]int, len(frames))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		fa, fb := frames[order[a]], frames[order[b]]
		if allTimed && !fa.Time.Equal(fb.Time) {
			return fa.Time.Before(fb.Time)
		}
		return fa.Key < fb.Key
	})
	var sets [][]int
	cur := []int{order[0]}
	flush := func() {
		if len(cur) > 1 {
			sets = append(sets, cur)
		}
	}
	for k := 1; k < len(order); k++ {
		prev, next := frames[order[k-1]], frames[order[k]]
		if linked(prev, next, o) && len(cur) < MaxSequence {
			cur = append(cur, order[k])
			continue
		}
		flush()
		cur = []int{order[k]}
	}
	flush()
	return sets
}

func linked(a, b Frame, o Options) bool {
	if len(a.Look) == 0 || len(b.Look) == 0 || LookDistance(a.Look, b.Look) > o.MaxLook {
		return false
	}
	if a.HasTime && b.HasTime {
		d := b.Time.Sub(a.Time)
		return d >= 0 && d <= o.Gap
	}
	return true
}

// ScoreOrder orders a set best first by the frames' own scores: evaluated, then
// sharpness, open eyes, composition, exposure; capture order on a tie.
func ScoreOrder(frames []Frame, set []int) []int {
	out := append([]int(nil), set...)
	sort.SliceStable(out, func(a, b int) bool { return better(frames[out[a]].Score, frames[out[b]].Score) })
	return out
}

func better(a, b Score) bool {
	switch {
	case a.Evaluated != b.Evaluated:
		return a.Evaluated
	case a.Sharp != b.Sharp:
		return a.Sharp > b.Sharp
	case a.EyesOpen != b.EyesOpen:
		return a.EyesOpen
	case a.Comp != b.Comp:
		return a.Comp > b.Comp
	}
	return a.Exp > b.Exp
}

// LookSize is the look fingerprint's grid: LookSize×LookSize cells of mean RGB.
const LookSize = 8

// LookDistance compares two look fingerprints, in [0,1]. Each grid is divided by
// its own mean brightness (about ±1 stop of exposure drops out; see levelled), then
// for shifts of up to one cell (about 12% reframing) the per-cell colour difference
// (mean over the 3 channels) is computed over the overlapping cells. A shift's score
// is the mean of the largest quarter of those cell differences, not the mean over
// every cell: a local change — a different subject in an otherwise-matching scene,
// say — is diluted to near zero by dozens of unchanged background cells if averaged
// over the whole grid, but survives in the worst quarter. Taking the smallest score
// over the 9 shifts still lets small reframing align through the search.
// A malformed look (not LookSize×LookSize×3 bytes, as from a hand-edited report)
// is as far as looks go: 1.
func LookDistance(a, b []uint8) float64 {
	if len(a) != LookSize*LookSize*3 || len(b) != len(a) {
		return 1
	}
	na, nb := levelled(a), levelled(b)
	best := 1.0
	var diffs []float64
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			diffs = diffs[:0]
			for y := 0; y < LookSize; y++ {
				for x := 0; x < LookSize; x++ {
					x2, y2 := x+dx, y+dy
					if x2 < 0 || y2 < 0 || x2 >= LookSize || y2 >= LookSize {
						continue
					}
					sum := 0.0
					for c := 0; c < 3; c++ {
						sum += math.Abs(na[(y*LookSize+x)*3+c] - nb[(y2*LookSize+x2)*3+c])
					}
					diffs = append(diffs, sum/3)
				}
			}
			if len(diffs) == 0 {
				continue
			}
			sort.Float64s(diffs)
			k := (len(diffs) + 3) / 4 // top quarter, rounded up
			sum := 0.0
			for _, d := range diffs[len(diffs)-k:] {
				sum += d
			}
			if score := sum / float64(k); score < best {
				best = score
			}
		}
	}
	return math.Min(1, best)
}

// levelled divides a grid by its mean and scales it so typical differences fall
// in [0,1].
func levelled(g []uint8) []float64 {
	mean := 0.0
	for _, v := range g {
		mean += float64(v)
	}
	mean = math.Max(1, mean/float64(len(g)))
	out := make([]float64, len(g))
	for i, v := range g {
		out[i] = math.Min(1, float64(v)/mean/4)
	}
	return out
}
