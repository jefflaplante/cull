package focus

import (
	"image"
	"sort"
)

const (
	CellSize   = 384 // grid cell for the "where focus landed" search, native px
	LandedSize = 768 // crop sent to the model around a chosen cell
)

// Cell is a candidate "where focus landed" region.
type Cell struct {
	Crop  image.Rectangle // LandedSize square around the grid cell, inside the frame
	Ratio float64
}

// Ratio is fine-scale over coarse-scale detail in r: the variance of the
// 4-neighbour Laplacian at native resolution, less the frame's noise variance,
// divided by the same after a 4× box downsample. Defocus removes fine detail but
// keeps coarse structure, so a blurred high-contrast edge scores far below a sharp
// low-contrast one: unlike raw Laplacian variance, the ratio doesn't reward
// contrast. Subtracting noise matters on real previews, where sensor noise would
// otherwise make smooth bokeh look "detailed".
func Ratio(luma []uint8, stride int, r image.Rectangle, noise float64) float64 {
	fine, coarse, _ := fineCoarse(luma, stride, r)
	return ratio(fine, coarse, noise)
}

func ratio(fine, coarse, noise float64) float64 {
	return max(fine-noise, 0) / (coarse + 1e-6)
}

// Landed returns up to k cells with the highest Ratio among cells that have real
// structure, skipping cells that overlap exclude (the subject crop), and the
// frame's noise estimate (fine-scale variance of its flattest tenth of cells,
// among those neither clipped nor flat) so
// callers can score other regions on the same scale.
//
// Eligibility is the most structured quarter of the frame by coarse detail, and
// never below a noise floor of median(fine)/4 (noise has coarse ≈ fine/16). The
// quarter matters on real previews: smooth bokeh with sensor noise has a higher
// ratio than sharp skin or fabric, and only its weak structure keeps it out.
func Landed(luma []uint8, w, h int, exclude image.Rectangle, k int) ([]Cell, float64) {
	type cell struct {
		r            image.Rectangle
		fine, coarse float64
	}
	var cells []cell
	var fines, measurable []float64
	for y := 0; y+CellSize <= h; y += CellSize {
		for x := 0; x+CellSize <= w; x += CellSize {
			r := image.Rect(x, y, x+CellSize, y+CellSize)
			f, c, m := fineCoarse(luma, w, r)
			cells = append(cells, cell{r, f, c})
			fines = append(fines, f)
			// Clipped highlights, crushed shadows and JPEG-flattened sky have no
			// noise left to measure: counting them would pull the estimate to 0.
			if f > 0 && m >= 16 && m <= 239 {
				measurable = append(measurable, f)
			}
		}
	}
	if len(cells) == 0 {
		return nil, 0
	}
	sort.Float64s(fines)
	if len(measurable) == 0 {
		measurable = fines
	}
	sort.Float64s(measurable)
	// Noise = fine variance of the flattest tenth of the cells where noise is
	// measurable. On grids of ten cells or fewer that is the flattest such cell,
	// whose own ratio is then 0; real 60MP frames have ~400 cells, so this only
	// shows on tiny previews and tests.
	noise := measurable[len(measurable)/10]
	if k <= 0 {
		return nil, noise
	}
	coarses := make([]float64, len(cells))
	for i, c := range cells {
		coarses[i] = c.coarse
	}
	sort.Float64s(coarses)
	// Most structured quarter of the frame, and above the noise floor.
	floor := max(fines[len(fines)/2]/4, coarses[len(coarses)-max(1, len(coarses)/4)])

	var out []Cell
	for _, c := range cells {
		if c.coarse > 0 && c.coarse >= floor && !c.r.Overlaps(exclude) {
			centre := image.Pt((c.r.Min.X+c.r.Max.X)/2, (c.r.Min.Y+c.r.Max.Y)/2)
			out = append(out, Cell{Crop: centered(centre, LandedSize, w, h), Ratio: ratio(c.fine, c.coarse, noise)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ratio > out[j].Ratio })
	if len(out) > k {
		out = out[:k]
	}
	return out, noise
}

func fineCoarse(luma []uint8, stride int, r image.Rectangle) (fine, coarse, mean float64) {
	fine = lapVar8(luma, stride, r.Min.X, r.Min.Y, r.Dx(), r.Dy())
	cw, ch := r.Dx()/4, r.Dy()/4
	down := make([]float32, cw*ch)
	total := 0
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			s := 0
			for dy := 0; dy < 4; dy++ {
				row := luma[(r.Min.Y+4*y+dy)*stride+r.Min.X+4*x:]
				s += int(row[0]) + int(row[1]) + int(row[2]) + int(row[3])
			}
			down[y*cw+x] = float32(s) / 16
			total += s
		}
	}
	coarse = lapVar(down, cw, 0, 0, cw, ch)
	if cw*ch > 0 {
		mean = float64(total) / float64(cw*ch*16)
	}
	return fine, coarse, mean
}

// lapVar8 is lapVar over 8-bit luma.
func lapVar8(g []uint8, stride, x0, y0, w, h int) float64 {
	var sum, sumSq float64
	n := 0
	for y := y0 + 1; y < y0+h-1; y++ {
		for x := x0 + 1; x < x0+w-1; x++ {
			i := y*stride + x
			v := float64(4*int(g[i]) - int(g[i-1]) - int(g[i+1]) - int(g[i-stride]) - int(g[i+stride]))
			sum += v
			sumSq += v * v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	m := sum / float64(n)
	return sumSq/float64(n) - m*m
}

// lapVar is the variance of the 4-neighbour Laplacian over the interior of the
// w×h region at (x0, y0).
func lapVar(g []float32, stride, x0, y0, w, h int) float64 {
	var sum, sumSq float64
	n := 0
	for y := y0 + 1; y < y0+h-1; y++ {
		for x := x0 + 1; x < x0+w-1; x++ {
			i := y*stride + x
			v := float64(4*g[i] - g[i-1] - g[i+1] - g[i-stride] - g[i+stride])
			sum += v
			sumSq += v * v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	m := sum / float64(n)
	return sumSq/float64(n) - m*m
}
