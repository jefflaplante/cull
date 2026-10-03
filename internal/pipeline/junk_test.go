package pipeline

import (
	"image"
	"image/color"
	"path/filepath"
	"testing"
)

func TestPrepareRecordsJunk(t *testing.T) {
	dir := t.TempDir()
	black := filepath.Join(dir, "L1.DNG")
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1067))
	for i := range img.Pix {
		img.Pix[i] = 4
	}
	dngWith(t, black, img)
	p, err := prepareFrame(cfg(dir), black)
	if err != nil {
		t.Fatal(err)
	}
	j := p.res.Junk
	if j == nil || j.Kind != "black" || j.DarkPct < 98 {
		t.Fatalf("junk %+v stats %+v", j, p.res.Stats)
	}

	real := filepath.Join(dir, "L2.DNG")
	g := image.NewRGBA(image.Rect(0, 0, 1600, 1067))
	for y := 0; y < 1067; y++ {
		for x := 0; x < 1600; x++ {
			v := uint8(x * 255 / 1600)
			g.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	dngWith(t, real, g)
	if p, _ := prepareFrame(cfg(dir), real); p.res.Junk != nil {
		t.Fatalf("a gradient flagged junk: %+v", p.res.Junk)
	}
}
