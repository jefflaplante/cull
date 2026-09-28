// Package group finds bursts and near-duplicates: consecutive frames close in
// capture time whose difference hashes are nearly identical.
package group

import (
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
