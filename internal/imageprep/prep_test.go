package imageprep

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// marked is a grey w×h JPEG with a 6×6 red block at stored top-left.
func marked(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{128, 128, 128, 255}
			if x < 6 && y < 6 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100})
	return buf.Bytes()
}

func isRed(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r>>8 > 200 && g>>8 < 90 && b>>8 < 90
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestFrameIsOrientedForDisplay(t *testing.T) {
	const w, h = 40, 24
	cases := []struct {
		o          int
		dw, dh     int
		redX, redY int // a pixel inside the displayed marker
	}{
		{1, w, h, 2, 2},
		{3, w, h, w - 3, h - 3},
		{6, h, w, h - 3, 2}, // 90° CW: stored top-left -> displayed top-right
		{8, h, w, 2, w - 3}, // 90° CCW: stored top-left -> displayed bottom-left
	}
	for _, c := range cases {
		f, err := Decode(marked(w, h), c.o)
		if err != nil {
			t.Fatal(err)
		}
		if f.W != c.dw || f.H != c.dh || len(f.Luma) != f.W*f.H {
			t.Fatalf("o=%d: %dx%d luma=%d", c.o, f.W, f.H, len(f.Luma))
		}
		// Luma: red is much darker than the grey around it.
		if y := f.Luma[c.redY*f.W+c.redX]; y > 100 {
			t.Errorf("o=%d: luma at marker %d, want dark", c.o, y)
		}
		if y := f.Luma[(f.H/2)*f.W+f.W/2]; y < 120 || y > 136 {
			t.Errorf("o=%d: luma at centre %d, want grey", c.o, y)
		}
		// Full frame, unscaled.
		full := decodeJPEG(t, mustBytes(f.Downscaled(10000, 100)))
		if full.Bounds().Dx() != c.dw || !isRed(full.At(c.redX, c.redY)) || isRed(full.At(f.W/2, f.H/2)) {
			t.Errorf("o=%d: downscaled frame %v, marker pixel %v", c.o, full.Bounds(), full.At(c.redX, c.redY))
		}
		// A display-space crop around the marker.
		r := image.Rect(c.redX-2, c.redY-2, c.redX+2, c.redY+2)
		crop := decodeJPEG(t, mustBytes(f.Crop(r, 100)))
		if crop.Bounds().Dx() != 4 || crop.Bounds().Dy() != 4 || !isRed(crop.At(2, 2)) {
			t.Errorf("o=%d: crop %v centre %v", c.o, crop.Bounds(), crop.At(2, 2))
		}
	}
}

func mustBytes(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func gradientJPEG(w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{uint8(x * 256 / w)})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100})
	return buf.Bytes()
}

func TestMeasureAndEncodeIncludingGrayscaleJPEG(t *testing.T) {
	f, err := Decode(gradientJPEG(2048, 1024), 1) // Go encodes *image.Gray as a 1-component JPEG
	if err != nil {
		t.Fatal(err)
	}
	s := Measure(f)
	if s.LumaP50 < 120 || s.LumaP50 > 135 || s.LumaP1 > 6 || s.LumaP99 < 248 || s.HighlightClipPct <= 0 || s.ShadowClipPct <= 0 {
		t.Fatalf("stats %+v", s)
	}

	full, err := f.Downscaled(1024, 85)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(full)); cfg.Width != 1024 || cfg.Height != 512 {
		t.Fatalf("downscaled %dx%d", cfg.Width, cfg.Height)
	}
	crop, err := f.Crop(image.Rect(2000, 1000, 2100, 1100), 90) // partly outside: clipped to the frame
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(crop)); cfg.Width != 48 || cfg.Height != 24 {
		t.Fatalf("crop %dx%d", cfg.Width, cfg.Height)
	}
	if _, err := f.Crop(image.Rect(5000, 5000, 5100, 5100), 90); err == nil {
		t.Fatal("crop fully outside the frame should fail")
	}
}

func TestDownLuma(t *testing.T) {
	luma := make([]uint8, 4000*2000)
	for i := range luma {
		luma[i] = 100
	}
	g, dw, dh, scale := DownLuma(luma, 4000, 2000, 2000)
	if dw != 2000 || dh != 1000 || scale != 0.5 || len(g) != dw*dh || g[0] != 100 || g[len(g)-1] != 100 {
		t.Fatalf("dw=%d dh=%d scale=%v len=%d g0=%d", dw, dh, scale, len(g), g[0])
	}
	g, dw, dh, scale = DownLuma(luma[:100*50], 100, 50, 2000)
	if dw != 100 || dh != 50 || scale != 1 || len(g) != 5000 {
		t.Fatalf("no-op downscale: dw=%d dh=%d scale=%v", dw, dh, scale)
	}
}
