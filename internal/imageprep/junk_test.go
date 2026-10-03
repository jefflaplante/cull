package imageprep

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"testing"
)

// lumaJPEG encodes a w×h colour JPEG whose grey level at (x, y) is fill(x, y).
func lumaJPEG(w, h int, fill func(x, y int) uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := fill(x, y)
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	return buf.Bytes()
}

// ellipse is true inside an ellipse centred in a w×h frame covering about frac of it.
func ellipse(w, h int, frac float64) func(x, y int) bool {
	// area of ellipse = pi*a*b; a/b = w/h
	a := float64(w) * math.Sqrt(frac/math.Pi)
	b := float64(h) * math.Sqrt(frac/math.Pi)
	return func(x, y int) bool {
		dx, dy := (float64(x)-float64(w)/2)/a, (float64(y)-float64(h)/2)/b
		return dx*dx+dy*dy <= 1
	}
}

func TestJunkClassify(t *testing.T) {
	const w, h = 1024, 683
	rng := rand.New(rand.NewSource(1))
	in := ellipse(w, h, 0.25)
	cases := []struct {
		name string
		fill func(x, y int) uint8
		want string
	}{
		{"black", func(x, y int) uint8 { return 5 }, "black"},
		{"white", func(x, y int) uint8 { return 252 }, "white"},
		{"flat grey", func(x, y int) uint8 { return 128 }, "uniform"},
		{"grey with sensor noise", func(x, y int) uint8 { return uint8(127 + rng.Intn(3)) }, "uniform"},
		{"low-key portrait", func(x, y int) uint8 {
			if in(x, y) {
				return 90
			}
			return 10
		}, ""},
		{"high-key portrait", func(x, y int) uint8 {
			if in(x, y) {
				return 120
			}
			return 245
		}, ""},
		{"97% black, 3% bright", func(x, y int) uint8 {
			if x < w*3/100 {
				return 200
			}
			return 5
		}, ""},
		{"99% black, 1% grey", func(x, y int) uint8 {
			if x < w/100 {
				return 60
			}
			return 5
		}, "black"},
		{"horizon gradient", func(x, y int) uint8 { return uint8(y * 255 / h) }, ""},
	}
	for _, c := range cases {
		f, err := Decode(lumaJPEG(w, h, c.fill), 1)
		if err != nil {
			t.Fatal(err)
		}
		s := Measure(f)
		if got := Junk(s); got != c.want {
			t.Errorf("%s: Junk = %q, want %q (dark %.2f%% bright %.2f%% contrast %.2f)", c.name, got, c.want, s.DarkPct, s.BrightPct, s.Contrast)
		}
	}
}

// Each threshold: just inside flags, just outside doesn't.
func TestJunkThresholds(t *testing.T) {
	cases := []struct {
		s    Stats
		want string
	}{
		{Stats{DarkPct: JunkDarkPct, Contrast: 50}, "black"},
		{Stats{DarkPct: JunkDarkPct - 0.01, Contrast: 50}, ""},
		{Stats{BrightPct: JunkBrightPct, Contrast: 50}, "white"},
		{Stats{BrightPct: JunkBrightPct - 0.01, Contrast: 50}, ""},
		{Stats{Contrast: JunkContrast - 0.01}, "uniform"},
		{Stats{Contrast: JunkContrast}, ""},
		{Stats{DarkPct: 99, Contrast: 0.5}, "black"}, // black wins over uniform
	}
	for _, c := range cases {
		if got := Junk(c.s); got != c.want {
			t.Errorf("%+v: %q, want %q", c.s, got, c.want)
		}
	}
}
