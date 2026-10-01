package focus

import (
	"image"
	"math/rand"
	"testing"
)

func TestSubjectRect(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		w, h int
		want image.Rectangle
	}{
		{"corner", Target{Center: image.Pt(0, 0), Size: 900}, 4000, 3000, image.Rect(0, 0, 900, 900)},
		{"small grows to min", Target{Center: image.Pt(2000, 1500), Size: 300}, 4000, 3000, image.Rect(1616, 1116, 2384, 1884)},
		{"large capped", Target{Center: image.Pt(2000, 1500), Size: 5000}, 4000, 3000, image.Rect(1232, 732, 2768, 2268)},
		{"frame smaller than min", Target{Center: image.Pt(300, 200), Size: 900}, 600, 400, image.Rect(100, 0, 500, 400)},
		{"centre beyond edge", Target{Center: image.Pt(9000, -50), Size: 800}, 4000, 3000, image.Rect(3200, 0, 4000, 800)},
	}
	for _, c := range cases {
		got := SubjectRect(c.t, c.w, c.h)
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
		if !got.In(image.Rect(0, 0, c.w, c.h)) {
			t.Errorf("%s: %v outside frame", c.name, got)
		}
	}
}

// blocks fills a w×h luma plane with random 8px blocks of lo/hi values: texture
// with structure at both native and 4×-downsampled scale.
func blocks(rng *rand.Rand, w, h int, lo, hi float32) []float32 {
	g := make([]float32, w*h)
	for by := 0; by < h; by += 8 {
		for bx := 0; bx < w; bx += 8 {
			v := lo
			if rng.Intn(2) == 0 {
				v = hi
			}
			for y := by; y < by+8 && y < h; y++ {
				for x := bx; x < bx+8 && x < w; x++ {
					g[y*w+x] = v
				}
			}
		}
	}
	return g
}

// blur applies a (2r+1)² box blur `passes` times to the region r of g.
func blur(g []float32, w int, rect image.Rectangle, rad, passes int) {
	for p := 0; p < passes; p++ {
		src := append([]float32(nil), g...)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				var sum float32
				n := 0
				for dy := -rad; dy <= rad; dy++ {
					for dx := -rad; dx <= rad; dx++ {
						xx, yy := x+dx, y+dy
						if xx >= rect.Min.X && xx < rect.Max.X && yy >= rect.Min.Y && yy < rect.Max.Y {
							sum += src[yy*w+xx]
							n++
						}
					}
				}
				g[y*w+x] = sum / float32(n)
			}
		}
	}
}

func TestRatioPrefersSharpOverBlurredSameTexture(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const w, h = 768, 384
	gf := blocks(rng, w, h, 60, 200)
	blur(gf, w, image.Rect(384, 0, 768, 384), 1, 2)
	g := u8(gf)
	sharp := Ratio(g, w, image.Rect(0, 0, 384, 384), 0)
	soft := Ratio(g, w, image.Rect(384, 0, 768, 384), 0)
	if !(sharp > 2*soft) {
		t.Fatalf("sharp ratio %.4f not clearly above blurred %.4f", sharp, soft)
	}
}

// scene: 8×4 cells of 384px. Mostly smooth noisy background (like bokeh), one
// high-contrast out-of-focus patch (sunlit litter) and one low-contrast sharp
// patch (the subject). This is the failure seen on real M11-P frames.
func scene(t *testing.T) (lum []uint8, w, h int, sharpPatch, oofPatch image.Rectangle) {
	t.Helper()
	rng := rand.New(rand.NewSource(2))
	w, h = 8*CellSize, 4*CellSize
	luma := make([]float32, w*h)
	for i := range luma {
		luma[i] = 110 + float32(rng.NormFloat64()*2)
	}
	oofPatch = image.Rect(0, 2*CellSize, 2*CellSize, 4*CellSize)
	sharpPatch = image.Rect(5*CellSize, CellSize, 7*CellSize, 3*CellSize)
	paint := func(r image.Rectangle, lo, hi float32) {
		tex := blocks(rng, r.Dx(), r.Dy(), lo, hi)
		for y := 0; y < r.Dy(); y++ {
			copy(luma[(r.Min.Y+y)*w+r.Min.X:], tex[y*r.Dx():(y+1)*r.Dx()])
		}
	}
	paint(oofPatch, 0, 255)
	blur(luma, w, oofPatch, 1, 1) // mild defocus, full contrast: sunlit litter
	paint(sharpPatch, 100, 120)   // in focus, low contrast: skin, fabric
	return u8(luma), w, h, sharpPatch, oofPatch
}

// u8 quantizes a synthetic float scene to the 8-bit luma the pipeline uses.
func u8(f []float32) []uint8 {
	out := make([]uint8, len(f))
	for i, v := range f {
		out[i] = uint8(min(max(v+0.5, 0), 255))
	}
	return out
}

func TestLandedPicksLowContrastSharpOverHighContrastBlur(t *testing.T) {
	luma, w, h, sharpPatch, oofPatch := scene(t)
	// Premise: raw Laplacian variance (the old tile picker) prefers the blurred patch.
	if fs, _, _ := fineCoarse(luma, w, sharpPatch); !(fineOf(luma, w, oofPatch) > fs) {
		t.Fatalf("scene no longer reproduces the failure: raw variance sharp=%.1f oof=%.1f", fs, fineOf(luma, w, oofPatch))
	}
	cells, _ := Landed(luma, w, h, image.Rectangle{}, 2)
	if len(cells) != 2 {
		t.Fatalf("want 2 cells, got %d", len(cells))
	}
	for _, c := range cells {
		centre := image.Pt((c.Crop.Min.X+c.Crop.Max.X)/2, (c.Crop.Min.Y+c.Crop.Max.Y)/2)
		if !centre.In(sharpPatch) || centre.In(oofPatch) {
			t.Errorf("landed crop %v (ratio %.3f) not centred in the sharp patch %v", c.Crop, c.Ratio, sharpPatch)
		}
		if c.Crop.Dx() != LandedSize || c.Crop.Dy() != LandedSize || !c.Crop.In(image.Rect(0, 0, w, h)) {
			t.Errorf("crop %v not a %dpx square inside the frame", c.Crop, LandedSize)
		}
	}
}

func TestLandedExcludesSubjectAndHandlesEdgeCases(t *testing.T) {
	luma, w, h, sharpPatch, _ := scene(t)
	excluded, _ := Landed(luma, w, h, sharpPatch, 3)
	for _, c := range excluded {
		cell := image.Rect(0, 0, CellSize, CellSize).Add(image.Pt(
			(c.Crop.Min.X+c.Crop.Max.X)/2/CellSize*CellSize, (c.Crop.Min.Y+c.Crop.Max.Y)/2/CellSize*CellSize))
		if cell.Overlaps(sharpPatch) {
			t.Errorf("cell %v overlaps the excluded subject %v", cell, sharpPatch)
		}
	}
	if got, _ := Landed(luma, w, h, image.Rectangle{}, 0); len(got) != 0 {
		t.Errorf("k=0: got %d cells", len(got))
	}
	if got, _ := Landed(make([]uint8, 300*200), 300, 200, image.Rectangle{}, 1); len(got) != 0 {
		t.Errorf("frame smaller than a cell: got %d cells", len(got))
	}
	if got, _ := Landed(make([]uint8, w*h), w, h, image.Rectangle{}, 1); len(got) != 0 {
		t.Errorf("flat frame has no structure: got %d cells", len(got))
	}
}

func fineOf(luma []uint8, w int, r image.Rectangle) float64 {
	f, _, _ := fineCoarse(luma, w, r)
	return f
}

// bokehScene reproduces the failure seen on real previews: most of the frame is
// smooth out-of-focus bokeh whose blotchy low-frequency structure plus sensor
// noise gives a high fine/coarse ratio, and a smaller sharp low-contrast subject.
func bokehScene(t *testing.T) (lum []uint8, w, h int, sharpPatch image.Rectangle) {
	t.Helper()
	rng := rand.New(rand.NewSource(4))
	w, h = 8*CellSize, 4*CellSize
	luma := blocks(rng, w, h, 90, 150)
	blur(luma, w, image.Rect(0, 0, w, h), 3, 2) // smooth blotches
	for i := range luma {
		luma[i] += float32(rng.NormFloat64() * 2) // sensor noise
	}
	sharpPatch = image.Rect(5*CellSize, CellSize, 7*CellSize, 3*CellSize)
	tex := blocks(rng, sharpPatch.Dx(), sharpPatch.Dy(), 100, 120)
	for y := 0; y < sharpPatch.Dy(); y++ {
		copy(luma[(sharpPatch.Min.Y+y)*w+sharpPatch.Min.X:], tex[y*sharpPatch.Dx():(y+1)*sharpPatch.Dx()])
	}
	return u8(luma), w, h, sharpPatch
}

func TestLandedIgnoresNoisyBokeh(t *testing.T) {
	luma, w, h, sharpPatch := bokehScene(t)
	bokeh := image.Rect(0, 0, CellSize, CellSize)
	// Premise: the ratio alone prefers the bokeh, and it clears a noise floor.
	fb, cb, _ := fineCoarse(luma, w, bokeh)
	fs, cs, _ := fineCoarse(luma, w, image.Rect(sharpPatch.Min.X, sharpPatch.Min.Y, sharpPatch.Min.X+CellSize, sharpPatch.Min.Y+CellSize))
	if !(fb/cb > fs/cs) || !(cb >= fb/4) {
		t.Fatalf("scene no longer reproduces the failure: bokeh fine=%.1f coarse=%.1f, sharp fine=%.1f coarse=%.1f", fb, cb, fs, cs)
	}
	cells, noise := Landed(luma, w, h, image.Rectangle{}, 2)
	if noise <= 0 {
		t.Fatalf("noise estimate %.1f: the scene has sensor noise", noise)
	}
	for _, c := range cells {
		centre := image.Pt((c.Crop.Min.X+c.Crop.Max.X)/2, (c.Crop.Min.Y+c.Crop.Max.Y)/2)
		if !centre.In(sharpPatch) {
			t.Errorf("landed crop %v (ratio %.3f) is bokeh, not the sharp patch %v", c.Crop, c.Ratio, sharpPatch)
		}
	}
}

func TestRatioSubtractsNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	const w, h = 384, 384
	gf := make([]float32, w*h)
	for i := range gf {
		gf[i] = 120 + float32(rng.NormFloat64()*2)
	}
	g := u8(gf)
	r := image.Rect(0, 0, w, h)
	raw := Ratio(g, w, r, 0)
	fine, _, _ := fineCoarse(g, w, r)
	if corrected := Ratio(g, w, r, fine); !(raw > 1) || corrected != 0 {
		t.Fatalf("pure noise: raw ratio %.2f (want > 1), noise-corrected %.3f (want 0)", raw, corrected)
	}
}

// Half the frame blown to white (flat, clipped): the noise estimate must come
// from the half that has noise, not collapse to 0.
func TestLandedNoiseIgnoresClippedCells(t *testing.T) {
	w, h := CellSize*8, CellSize*4
	luma := make([]uint8, w*h)
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				luma[y*w+x] = 255
			} else {
				luma[y*w+x] = uint8(128 + rng.Intn(9) - 4)
			}
		}
	}
	_, noise := Landed(luma, w, h, image.Rectangle{}, 0)
	all := make([]uint8, w*h) // the same noise over the whole frame
	rng = rand.New(rand.NewSource(1))
	for i := range all {
		all[i] = uint8(128 + rng.Intn(9) - 4)
	}
	_, want := Landed(all, w, h, image.Rectangle{}, 0)
	if noise < want/2 {
		t.Fatalf("noise %v on a half-clipped frame, %v without clipping", noise, want)
	}
}

func TestEyeWindowsAroundPupils(t *testing.T) {
	tg := Target{Center: image.Pt(500, 400), Size: 200, Pupils: []image.Point{{460, 400}, {540, 400}}}
	ws := EyeWindows(tg, 1600, 1067)
	if len(ws) != 2 || ws[0].Dx() != 60 || !image.Pt(460, 400).In(ws[0]) || !ws[1].In(image.Rect(0, 0, 1600, 1067)) {
		t.Fatalf("%v", ws)
	}
	edge := Target{Center: image.Pt(10, 10), Size: 50, Pupils: []image.Point{{2, 3}}} // tiny face at the corner
	if ws := EyeWindows(edge, 1600, 1067); len(ws) != 1 || ws[0].Dx() < 32 || !ws[0].In(image.Rect(0, 0, 1600, 1067)) {
		t.Fatalf("%v", ws)
	}
}
