// Package group finds bursts and near-duplicates: consecutive frames close in
// capture time whose difference hashes are nearly identical.
package group

import (
	"math"
	"math/bits"
	"sort"
	"time"
)

// DHash is a 64-bit difference hash: the image box-averaged to 9×8 luma cells,
// one bit per horizontal neighbour comparison. Robust to small shifts and
// exposure changes, sensitive to a different scene.
func DHash(luma []uint8, w, h int) uint64 {
	var cells [8][9]float64
	for cy := 0; cy < 8; cy++ {
		y0, y1 := cy*h/8, (cy+1)*h/8
		for cx := 0; cx < 9; cx++ {
			x0, x1 := cx*w/9, (cx+1)*w/9
			sum, n := 0, 0
			for y := y0; y < y1; y += 2 { // every other pixel is plenty for a 72-cell average
				row := luma[y*w:]
				for x := x0; x < x1; x += 2 {
					sum += int(row[x])
					n++
				}
			}
			if n > 0 {
				cells[cy][cx] = float64(sum) / float64(n)
			}
		}
	}
	var hash uint64
	for cy := 0; cy < 8; cy++ {
		for cx := 0; cx < 8; cx++ {
			hash <<= 1
			if cells[cy][cx] > cells[cy][cx+1] {
				hash |= 1
			}
		}
	}
	return hash
}

// Hamming is the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Score ranks frames within a group.
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
	Hash    uint64
	HasHash bool
	Score   Score
}

// MaxBurst caps a burst: real bursts rarely exceed a few seconds at 4.5 fps,
// and a runaway group would turn a session into "duplicates".
const MaxBurst = 15

// Options: Gap 0 disables grouping.
type Options struct {
	Gap        time.Duration // max time between consecutive frames of a burst
	MaxHamming int           // max dHash distance between consecutive frames
}

// Groups returns bursts of two or more frames as indices into frames, each in
// capture order. Frames are ordered by capture time, then key; a frame joins the
// current burst when it is within Gap of its neighbour (or both lack a time),
// within MaxHamming of both its neighbour and the burst's first frame, and the
// burst has fewer than MaxBurst frames.
func Groups(frames []Frame, o Options) [][]int {
	if o.Gap <= 0 || len(frames) < 2 {
		return nil
	}
	order := make([]int, len(frames))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		fa, fb := frames[order[a]], frames[order[b]]
		if fa.HasTime && fb.HasTime && !fa.Time.Equal(fb.Time) {
			return fa.Time.Before(fb.Time)
		}
		if fa.HasTime != fb.HasTime {
			return fa.HasTime // timed frames first; untimed ones by name after
		}
		return fa.Key < fb.Key
	})
	var groups [][]int
	cur := []int{order[0]}
	flush := func() {
		if len(cur) > 1 {
			groups = append(groups, cur)
		}
	}
	for k := 1; k < len(order); k++ {
		prev, next, first := frames[order[k-1]], frames[order[k]], frames[cur[0]]
		// A frame joins when it follows its neighbour closely AND still resembles
		// the burst's first frame: timestamps can be unreliable (rewritten on
		// export), and neighbour-only linking would chain a whole session.
		if linked(prev, next, o) && Hamming(first.Hash, next.Hash) <= o.MaxHamming && len(cur) < MaxBurst {
			cur = append(cur, order[k])
			continue
		}
		flush()
		cur = []int{order[k]}
	}
	flush()
	return groups
}

func linked(a, b Frame, o Options) bool {
	if !a.HasHash || !b.HasHash || Hamming(a.Hash, b.Hash) > o.MaxHamming {
		return false
	}
	if a.HasTime && b.HasTime {
		d := b.Time.Sub(a.Time)
		return d >= 0 && d <= o.Gap
	}
	return a.HasTime == b.HasTime // two untimed neighbours: the hash decides
}

// Best picks the frame to keep: evaluated first, then sharpness score, open
// eyes, composition, exposure; earliest on a tie.
func Best(frames []Frame, group []int) int {
	best := group[0]
	for _, i := range group[1:] {
		if better(frames[i].Score, frames[best].Score) {
			best = i
		}
	}
	return best
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
func LookDistance(a, b []uint8) float64 {
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
