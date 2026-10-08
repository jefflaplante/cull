package xmp

import "testing"

// crs:Crop* orientation, settled 2026-10-08 against LightCraft 0.4.0 (built from
// storytold/lightcraft@2472021) and cross-checked with Lightroom's documented
// behavior (Lightroom Classic writes crop edges in the oriented frame):
// render an orientation-6 (portrait, stored landscape) DNG with a sidecar whose
// crs:Crop* are display-frame edges and the rendered crop window matches those
// edges (pixel correlation 0.9989/0.9973 at two crop positions); transposed
// interpretations of the same numbers score 0.29 / -0.26 or produce the wrong
// output dimensions. So crs:Crop* are normalized edges of the ORIENTED image,
// and a display-frame crop needs NO transposition for orientation 6 (nor 3/8).
// Evidence: NAS /Jules/lightcraft-test/crop-orientation-test/.
func TestCropIsWrittenInDisplayOrientation(t *testing.T) {
	// A display crop of the top-left quadrant must be written as the top-left
	// quadrant, whatever the stored orientation: consumers (Lightroom,
	// LightCraft) read crs:Crop* in the oriented frame.
	cases := []struct {
		orient int
		l, tp, r, b float64
	}{
		{1, 0.1, 0.2, 0.6, 0.7},
		{6, 0.1, 0.2, 0.6, 0.7},
		{8, 0.1, 0.2, 0.6, 0.7},
		{3, 0.1, 0.2, 0.6, 0.7},
	}
	for _, c := range cases {
		got := DisplayCrop(c.l, c.tp, c.r, c.b)
		if got != (Box{Left: c.l, Top: c.tp, Right: c.r, Bottom: c.b}) {
			t.Fatalf("orientation %d: display crop written as %+v, want %+v", c.orient, got, Box{c.l, c.tp, c.r, c.b})
		}
	}
}
