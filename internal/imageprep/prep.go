// Package imageprep turns an embedded preview into model inputs: an oriented
// frame (RGBA + luma) that can be downscaled for composition/exposure and cropped
// at native resolution for focus, plus deterministic exposure measurements.
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
)

// Stats are measured on the oriented preview at native resolution.
// Note: the preview is a tone-mapped rendering; clipping here overstates raw clipping.
type Stats struct {
	MeanLuma         float64 `json:"mean_luma"`
	LumaP1           int     `json:"luma_p1"`
	LumaP50          int     `json:"luma_p50"`
	LumaP99          int     `json:"luma_p99"`
	HighlightClipPct float64 `json:"highlight_clip_pct"` // any channel >= 250
	ShadowClipPct    float64 `json:"shadow_clip_pct"`    // luma <= 3
}

// Options controls model-input preparation.
type Options struct {
	MaxEdge int // long edge of the full-frame image sent to the model
}

// Frame is a decoded preview oriented for display, with its Rec.709 luma plane.
type Frame struct {
	RGBA *image.RGBA
	Luma []float32
	W, H int
}

// Decode decodes a JPEG preview and applies EXIF orientation.
func Decode(jpegData []byte, orientation int) (*Frame, error) {
	src, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("decode preview: %w", err)
	}
	img := orient(toRGBA(src), orientation)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	return &Frame{RGBA: img, Luma: luma(img), W: w, H: h}, nil
}

// Measure computes exposure statistics.
func Measure(f *Frame) Stats { return measure(f.RGBA, f.Luma) }

// Downscaled encodes the whole frame with its long edge at most maxEdge.
func (f *Frame) Downscaled(maxEdge, quality int) ([]byte, error) {
	return encode(downscale(f.RGBA, maxEdge), quality)
}

// Crop encodes r (clipped to the frame) at native resolution.
func (f *Frame) Crop(r image.Rectangle, quality int) ([]byte, error) {
	r = r.Intersect(f.RGBA.Bounds())
	if r.Empty() {
		return nil, fmt.Errorf("crop %v outside frame %dx%d", r, f.W, f.H)
	}
	return encode(f.RGBA.SubImage(r), quality)
}

// DownLuma box-downscales a luma plane to 8-bit with its long edge at most
// maxEdge; scale = dw/w (1 when no downscale was needed).
func DownLuma(l []float32, w, h, maxEdge int) (g []uint8, dw, dh int, scale float64) {
	long := max(w, h)
	if maxEdge <= 0 || long <= maxEdge {
		g = make([]uint8, w*h)
		for i, v := range l {
			g[i] = clamp8(v)
		}
		return g, w, h, 1
	}
	inv := float64(long) / float64(maxEdge)
	dw, dh = int(math.Round(float64(w)/inv)), int(math.Round(float64(h)/inv))
	g = make([]uint8, dw*dh)
	for y := 0; y < dh; y++ {
		sy0, sy1 := span(y, inv, h)
		for x := 0; x < dw; x++ {
			sx0, sx1 := span(x, inv, w)
			var acc float64
			for sy := sy0; sy < sy1; sy++ {
				row := l[sy*w:]
				for sx := sx0; sx < sx1; sx++ {
					acc += float64(row[sx])
				}
			}
			g[y*dw+x] = clamp8(float32(acc / float64((sy1-sy0)*(sx1-sx0))))
		}
	}
	return g, dw, dh, float64(dw) / float64(w)
}

func clamp8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
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
