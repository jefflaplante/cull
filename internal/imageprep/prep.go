// Package imageprep turns an embedded preview into model inputs: an oriented
// frame that can be downscaled for composition/exposure and cropped at native
// resolution for focus, plus deterministic exposure measurements.
//
// A 60MP preview is kept as the decoder's YCbCr image in its stored orientation
// (~120 MB at 4:2:2) plus a display-oriented 8-bit luma plane (~60 MB). Crops and
// the downscaled frame are cut in stored coordinates and only the small result is
// converted to RGB and rotated, instead of rotating a full RGBA copy (~480 MB).
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
)

// Stats are measured on the preview at native resolution.
// Note: the preview is a tone-mapped rendering; clipping here overstates raw clipping.
type Stats struct {
	MeanLuma         float64 `json:"mean_luma"`
	LumaP1           int     `json:"luma_p1"`
	LumaP50          int     `json:"luma_p50"`
	LumaP99          int     `json:"luma_p99"`
	HighlightClipPct float64 `json:"highlight_clip_pct"` // any channel >= 250
	ShadowClipPct    float64 `json:"shadow_clip_pct"`    // luma <= 3
	// Junk filter measures (see Junk): share of near-black (luma < 16) and blown
	// (luma >= 250) pixels, and whole-frame contrast, the luma std of a 64 px thumbnail.
	DarkPct   float64 `json:"dark_pct"`
	BrightPct float64 `json:"bright_pct"`
	Contrast  float64 `json:"contrast"`
}

// Options controls model-input preparation.
type Options struct {
	MaxEdge int // long edge of the full-frame image sent to the model
}

// Frame is a decoded preview. W, H and Luma are in display orientation (after
// EXIF orientation); the pixels stay in stored orientation.
type Frame struct {
	Luma []uint8 // JPEG luma (Y), display orientation, W*H
	W, H int

	src *image.YCbCr // stored orientation
	o   int          // EXIF orientation: 1, 3, 6, 8 (others treated as 1)
}

// Decode decodes a JPEG preview and records its EXIF orientation.
func Decode(jpegData []byte, orientation int) (*Frame, error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("decode preview: %w", err)
	}
	src := asYCbCr(img)
	if orientation != 3 && orientation != 6 && orientation != 8 {
		orientation = 1
	}
	f := &Frame{src: src, o: orientation}
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	f.W, f.H = sw, sh
	if orientation == 6 || orientation == 8 {
		f.W, f.H = sh, sw
	}
	f.Luma = make([]uint8, f.W*f.H)
	for sy := 0; sy < sh; sy++ {
		row := src.Y[sy*src.YStride : sy*src.YStride+sw]
		for sx, v := range row {
			dx, dy := f.toDisplay(sx, sy)
			f.Luma[dy*f.W+dx] = v
		}
	}
	return f, nil
}

// asYCbCr returns baseline JPEG output as is; grayscale gets neutral chroma and
// anything else (CMYK) is converted per pixel.
func asYCbCr(img image.Image) *image.YCbCr {
	switch m := img.(type) {
	case *image.YCbCr:
		if m.Rect.Min == (image.Point{}) {
			return m
		}
	case *image.Gray:
		b := m.Bounds()
		out := image.NewYCbCr(image.Rect(0, 0, b.Dx(), b.Dy()), image.YCbCrSubsampleRatio420)
		for y := 0; y < b.Dy(); y++ {
			copy(out.Y[y*out.YStride:], m.Pix[y*m.Stride:y*m.Stride+b.Dx()])
		}
		for i := range out.Cb {
			out.Cb[i], out.Cr[i] = 128, 128
		}
		return out
	}
	b := img.Bounds()
	out := image.NewYCbCr(image.Rect(0, 0, b.Dx(), b.Dy()), image.YCbCrSubsampleRatio444)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			yy, cb, cr := color.RGBToYCbCr(uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			i := out.YOffset(x, y)
			out.Y[i], out.Cb[i], out.Cr[i] = yy, cb, cr
		}
	}
	return out
}

// toDisplay maps a stored pixel to display coordinates.
func (f *Frame) toDisplay(sx, sy int) (int, int) {
	sw, sh := f.src.Rect.Dx(), f.src.Rect.Dy()
	switch f.o {
	case 3:
		return sw - 1 - sx, sh - 1 - sy
	case 6: // rotate 90° CW for display
		return sh - 1 - sy, sx
	case 8: // rotate 90° CCW for display
		return sy, sw - 1 - sx
	}
	return sx, sy
}

// toStored maps a display pixel to stored coordinates (inverse of toDisplay).
func (f *Frame) toStored(dx, dy int) (int, int) {
	sw, sh := f.src.Rect.Dx(), f.src.Rect.Dy()
	switch f.o {
	case 3:
		return sw - 1 - dx, sh - 1 - dy
	case 6:
		return dy, sh - 1 - dx
	case 8:
		return sw - 1 - dy, dx
	}
	return dx, dy
}

// storedRect maps a non-empty display rectangle to the stored rectangle it covers.
func (f *Frame) storedRect(r image.Rectangle) image.Rectangle {
	ax, ay := f.toStored(r.Min.X, r.Min.Y)
	bx, by := f.toStored(r.Max.X-1, r.Max.Y-1)
	return image.Rect(min(ax, bx), min(ay, by), max(ax, bx)+1, max(ay, by)+1)
}

// rotate turns a stored-orientation RGBA into display orientation.
func (f *Frame) rotate(src *image.RGBA) *image.RGBA {
	if f.o == 1 {
		return src
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if f.o == 6 || f.o == 8 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch f.o {
			case 3:
				dx, dy = w-1-x, h-1-y
			case 6:
				dx, dy = h-1-y, x
			case 8:
				dx, dy = y, w-1-x
			}
			copy(dst.Pix[dst.PixOffset(dx, dy):dst.PixOffset(dx, dy)+4], src.Pix[src.PixOffset(x, y):src.PixOffset(x, y)+4])
		}
	}
	return dst
}

// Measure computes exposure statistics over every pixel.
func Measure(f *Frame) Stats {
	var hist [256]int
	var sum float64
	hi := 0
	s := f.src
	sw, sh := s.Rect.Dx(), s.Rect.Dy()
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			yy := s.Y[y*s.YStride+x]
			ci := s.COffset(x, y)
			r, g, b := color.YCbCrToRGB(yy, s.Cb[ci], s.Cr[ci])
			hist[yy]++
			sum += float64(yy)
			if r >= 250 || g >= 250 || b >= 250 {
				hi++
			}
		}
	}
	n := sw * sh
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
	shadow := hist[0] + hist[1] + hist[2] + hist[3]
	dark, bright := 0, 0
	for i := 0; i < 16; i++ {
		dark += hist[i]
	}
	for i := 250; i < 256; i++ {
		bright += hist[i]
	}
	return Stats{
		DarkPct:          round(100*float64(dark)/float64(n), 2),
		BrightPct:        round(100*float64(bright)/float64(n), 2),
		Contrast:         round(thumbContrast(f), 2),
		MeanLuma:         round(sum/float64(n), 1),
		LumaP1:           pct(0.01),
		LumaP50:          pct(0.50),
		LumaP99:          pct(0.99),
		HighlightClipPct: round(100*float64(hi)/float64(n), 2),
		ShadowClipPct:    round(100*float64(shadow)/float64(n), 2),
	}
}

// Grid returns an n×n grid of mean RGB (row-major, 3 bytes per cell) of the frame
// as displayed: a small colour-and-layout fingerprint for grouping similar frames.
func (f *Frame) Grid(n int) []uint8 {
	out := make([]uint8, 0, n*n*3)
	for cy := 0; cy < n; cy++ {
		for cx := 0; cx < n; cx++ {
			r := f.storedRect(image.Rect(cx*f.W/n, cy*f.H/n, (cx+1)*f.W/n, (cy+1)*f.H/n))
			var sy, sb, sr, cnt int
			for y := r.Min.Y; y < r.Max.Y; y += 4 {
				for x := r.Min.X; x < r.Max.X; x += 4 {
					yi, ci := f.src.YOffset(x, y), f.src.COffset(x, y)
					sy += int(f.src.Y[yi])
					sb += int(f.src.Cb[ci])
					sr += int(f.src.Cr[ci])
					cnt++
				}
			}
			if cnt == 0 {
				out = append(out, 0, 0, 0)
				continue
			}
			R, G, B := color.YCbCrToRGB(uint8(sy/cnt), uint8(sb/cnt), uint8(sr/cnt))
			out = append(out, R, G, B)
		}
	}
	return out
}

// Downscaled encodes the whole frame, display-oriented, long edge at most maxEdge.
func (f *Frame) Downscaled(maxEdge, quality int) ([]byte, error) {
	return encode(f.rotate(downscale(f.src, maxEdge)), quality)
}

// Crop encodes display rectangle r (clipped to the frame) at native resolution.
func (f *Frame) Crop(r image.Rectangle, quality int) ([]byte, error) {
	r = r.Intersect(image.Rect(0, 0, f.W, f.H))
	if r.Empty() {
		return nil, fmt.Errorf("crop outside frame %dx%d", f.W, f.H)
	}
	sr := f.storedRect(r)
	rgba := image.NewRGBA(image.Rect(0, 0, sr.Dx(), sr.Dy()))
	draw.Draw(rgba, rgba.Bounds(), f.src, sr.Min, draw.Src)
	return encode(f.rotate(rgba), quality)
}

// DownLuma box-downscales a luma plane with its long edge at most maxEdge;
// scale = dw/w (1 when no downscale was needed).
func DownLuma(l []uint8, w, h, maxEdge int) (g []uint8, dw, dh int, scale float64) {
	long := max(w, h)
	if maxEdge <= 0 || long <= maxEdge {
		return append([]uint8(nil), l...), w, h, 1
	}
	inv := float64(long) / float64(maxEdge)
	dw, dh = int(math.Round(float64(w)/inv)), int(math.Round(float64(h)/inv))
	g = make([]uint8, dw*dh)
	for y := 0; y < dh; y++ {
		sy0, sy1 := span(y, inv, h)
		for x := 0; x < dw; x++ {
			sx0, sx1 := span(x, inv, w)
			acc := 0
			for sy := sy0; sy < sy1; sy++ {
				row := l[sy*w:]
				for sx := sx0; sx < sx1; sx++ {
					acc += int(row[sx])
				}
			}
			g[y*dw+x] = uint8(acc / ((sy1 - sy0) * (sx1 - sx0)))
		}
	}
	return g, dw, dh, float64(dw) / float64(w)
}

// downscale is an area-average (box) filter from YCbCr straight to RGBA, in
// stored orientation, averaging Y, Cb and Cr before a single conversion.
func downscale(src *image.YCbCr, maxEdge int) *image.RGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	long := max(w, h)
	if maxEdge <= 0 || long <= maxEdge {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), src, src.Rect.Min, draw.Src)
		return dst
	}
	scale := float64(long) / float64(maxEdge)
	dw, dh := int(math.Round(float64(w)/scale)), int(math.Round(float64(h)/scale))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0, sy1 := span(y, scale, h)
		for x := 0; x < dw; x++ {
			sx0, sx1 := span(x, scale, w)
			var ay, acb, acr int
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					ci := src.COffset(sx, sy)
					ay += int(src.Y[sy*src.YStride+sx])
					acb += int(src.Cb[ci])
					acr += int(src.Cr[ci])
				}
			}
			n := (sy1 - sy0) * (sx1 - sx0)
			r, g, b := color.YCbCrToRGB(uint8(ay/n), uint8(acb/n), uint8(acr/n))
			d := dst.PixOffset(x, y)
			dst.Pix[d], dst.Pix[d+1], dst.Pix[d+2], dst.Pix[d+3] = r, g, b, 255
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
