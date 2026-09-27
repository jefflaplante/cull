package imageprep

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// Left half: fine checkerboard (high Laplacian variance). Right half: flat grey.
func synthetic(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := uint8(128)
			if x < w/2 && (x/2+y/2)%2 == 0 {
				c = 230
			} else if x < w/2 {
				c = 30
			}
			img.Set(x, y, color.RGBA{c, c, c, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	return buf.Bytes()
}

func TestPrepareSelectsDetailedTilesAndOrients(t *testing.T) {
	p, err := Prepare(synthetic(2048, 1024), 1, Options{MaxEdge: 1024, Tiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.TileRects {
		if r.Max.X > 1024 {
			t.Fatalf("tile %v not in detailed (left) half", r)
		}
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(p.FullFrame))
	if cfg.Width != 1024 || cfg.Height != 512 {
		t.Fatalf("full frame %dx%d", cfg.Width, cfg.Height)
	}

	p6, err := Prepare(synthetic(2048, 1024), 6, Options{MaxEdge: 1024, Tiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p6.Width != 1024 || p6.Height != 2048 {
		t.Fatalf("orientation 6 dims %dx%d", p6.Width, p6.Height)
	}
	// Stored left half becomes displayed top half after 90° CW rotation.
	if r := p6.TileRects[0]; r.Max.Y > 1024 {
		t.Fatalf("rotated tile %v not in top half", r)
	}
}
