// Package imageprep turns an embedded preview into model inputs: a downscaled full
// frame for composition/exposure, native-resolution crops of the highest-detail
// regions for sharpness, and deterministic measurements the model can anchor on.
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"sort"
)

// Stats are measured on the oriented preview at native resolution.
// Note: the preview is a tone-mapped rendering; clipping here overstates raw clipping.
type Stats struct {
	MeanLuma          float64 `json:"mean_luma"`
	LumaP1            int     `json:"luma_p1"`
	LumaP50           int     `json:"luma_p50"`
	LumaP99           int     `json:"luma_p99"`
	HighlightClipPct  float64 `json:"highlight_clip_pct"` // any channel >= 250
	ShadowClipPct     float64 `json:"shadow_clip_pct"`    // luma <= 3
	GlobalSharpness   float64 `json:"global_sharpness"`   // Laplacian variance, whole frame
	PeakTileSharpness float64 `json:"peak_tile_sharpness"`
}

// Prepared holds encoded model inputs.
type Prepared struct {
	FullFrame []byte
	Tiles     [][]byte
	TileRects []image.Rectangle
	Width     int // oriented preview dimensions
	Height    int
	Stats     Stats
}

// Options controls preparation.
type Options struct {
	MaxEdge int // long edge of the full-frame image sent to the model
	Tiles   int // number of native-resolution detail tiles
}

// Prepare decodes and processes a JPEG preview.
func Prepare(jpegData []byte, orientation int, opt Options) (*Prepared, error) {
	src, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("decode preview: %w", err)
	}
	img := orient(toRGBA(src), orientation)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	gray := luma(img)
	stats := measure(img, gray)
	stats.GlobalSharpness = lapVar(gray, w, 0, 0, w, h)

	rects, peak := sharpestTiles(gray, w, h, opt.Tiles)
	stats.PeakTileSharpness = peak

	p := &Prepared{Width: w, Height: h, Stats: stats, TileRects: rects}
	if p.FullFrame, err = encode(downscale(img, opt.MaxEdge), 85); err != nil {
		return nil, err
	}
	for _, r := range rects {
		b, err := encode(img.SubImage(r), 90)
		if err != nil {
			return nil, err
		}
		p.Tiles = append(p.Tiles, b)
	}
	return p, nil
}

func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// orient applies EXIF orientation 3/6/8. Mirrored orientations (2,4,5,7) don't
// occur on this camera and are passed through unchanged.
func orient(src *image.RGBA, o int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	var dst *image.RGBA
	var mapPx func(x, y int) (int, int)
	switch o {
	case 3:
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
		mapPx = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 6: // rotate 90° CW for display
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		mapPx = func(x, y int) (int, int) { return h - 1 - y, x }
	case 8: // rotate 90° CCW for display
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		mapPx = func(x, y int) (int, int) { return y, w - 1 - x }
	default:
		return src
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := mapPx(x, y)
			copy(dst.Pix[dst.PixOffset(dx, dy):dst.PixOffset(dx, dy)+4], src.Pix[src.PixOffset(x, y):src.PixOffset(x, y)+4])
		}
	}
	return dst
}

func luma(img *image.RGBA) []float32 {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	g := make([]float32, w*h)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			r, gg, b := float32(row[x*4]), float32(row[x*4+1]), float32(row[x*4+2])
			g[y*w+x] = 0.2126*r + 0.7152*gg + 0.0722*b
		}
	}
	return g
}

func measure(img *image.RGBA, gray []float32) Stats {
	var hist [256]int
	var sum float64
	hi := 0
	for i, v := range gray {
		l := int(v + 0.5)
		if l > 255 {
			l = 255
		}
		hist[l]++
		sum += float64(v)
		px := img.Pix[i*4 : i*4+3] // RGBA image built by toRGBA/orient has Stride == 4*w
		if px[0] >= 250 || px[1] >= 250 || px[2] >= 250 {
			hi++
		}
	}
	n := len(gray)
	pct := func(p float64) int {
		target := int(math.Ceil(p * float64(n)))
		acc := 0
		for i, c := range hist {
			acc += c
			if acc >= target {
				return i
			}
		}
		return 255
	}
	shadow := 0
	for i := 0; i <= 3; i++ {
		shadow += hist[i]
	}
	return Stats{
		MeanLuma:         round(sum/float64(n), 1),
		LumaP1:           pct(0.01),
		LumaP50:          pct(0.50),
		LumaP99:          pct(0.99),
		HighlightClipPct: round(100*float64(hi)/float64(n), 2),
		ShadowClipPct:    round(100*float64(shadow)/float64(n), 2),
	}
}

// lapVar is the variance of the 4-neighbour Laplacian over a region: a standard
// focus measure. It is relative (noise and in-camera sharpening raise it), so it is
// useful for ranking within a batch and for picking tiles, not as an absolute gate.
func lapVar(g []float32, stride, x0, y0, x1, y1 int) float64 {
	var sum, sumSq float64
	n := 0
	for y := y0 + 1; y < y1-1; y++ {
		for x := x0 + 1; x < x1-1; x++ {
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
	return round(sumSq/float64(n)-m*m, 1)
}

// sharpestTiles grids the frame and returns the k tiles with the highest Laplacian
// variance. With thin depth of field the in-focus region is where acuity must be
// judged; if even the best tile is soft, focus was missed.
func sharpestTiles(g []float32, w, h, k int) ([]image.Rectangle, float64) {
	if k <= 0 {
		return nil, 0
	}
	long := w
	if h > long {
		long = h
	}
	size := long / 8
	if size < 256 {
		size = 256
	}
	if size > 768 {
		size = 768
	}
	type scored struct {
		r image.Rectangle
		v float64
	}
	var tiles []scored
	for y := 0; y+size <= h; y += size {
		for x := 0; x+size <= w; x += size {
			tiles = append(tiles, scored{image.Rect(x, y, x+size, y+size), lapVar(g, w, x, y, x+size, y+size)})
		}
	}
	if len(tiles) == 0 {
		return []image.Rectangle{image.Rect(0, 0, w, h)}, lapVar(g, w, 0, 0, w, h)
	}
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].v > tiles[j].v })
	if k > len(tiles) {
		k = len(tiles)
	}
	out := make([]image.Rectangle, k)
	for i := range out {
		out[i] = tiles[i].r
	}
	return out, tiles[0].v
}

// downscale is an area-average (box) filter; adequate for downsampling and stdlib-only.
func downscale(src *image.RGBA, maxEdge int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	long := w
	if h > long {
		long = h
	}
	if maxEdge <= 0 || long <= maxEdge {
		return src
	}
	scale := float64(long) / float64(maxEdge)
	dw, dh := int(math.Round(float64(w)/scale)), int(math.Round(float64(h)/scale))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0, sy1 := span(y, scale, h)
		for x := 0; x < dw; x++ {
			sx0, sx1 := span(x, scale, w)
			var acc [4]uint64
			for sy := sy0; sy < sy1; sy++ {
				off := src.PixOffset(sx0, sy)
				for sx := sx0; sx < sx1; sx++ {
					acc[0] += uint64(src.Pix[off])
					acc[1] += uint64(src.Pix[off+1])
					acc[2] += uint64(src.Pix[off+2])
					acc[3] += uint64(src.Pix[off+3])
					off += 4
				}
			}
			n := uint64((sy1 - sy0) * (sx1 - sx0))
			d := dst.PixOffset(x, y)
			for c := 0; c < 4; c++ {
				dst.Pix[d+c] = uint8(acc[c] / n)
			}
		}
	}
	return dst
}

func span(i int, scale float64, limit int) (int, int) {
	a := int(float64(i) * scale)
	b := int(float64(i+1) * scale)
	if b > limit {
		b = limit
	}
	if b <= a {
		b = a + 1
	}
	return a, b
}

func encode(img image.Image, q int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
