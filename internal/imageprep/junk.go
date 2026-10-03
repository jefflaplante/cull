package imageprep

import "math"

// Junk thresholds, measured 2026-10-02 on 992 real M11-P frames and the 17 samples
// (see CLAUDE.md): the one blank frame scored 99.95% blown and contrast 0.3; the
// darkest real frame was 74% near-black, the brightest 82% blown, and the flattest
// (low-key portraits in shade) had contrast 10.0.
const (
	JunkDarkPct   = 98.0 // black: at least this share of pixels with luma < 16
	JunkBrightPct = 95.0 // white: at least this share with luma >= 250
	JunkContrast  = 3.0  // uniform: thumbnail luma std below this
)

// Junk classifies a frame that is unmistakably empty: "black" (lens cap, misfire),
// "white" (blown, flash misfire), "uniform" (flat at any brightness), or "" for a
// real picture. Detail-based rules were tried and dropped: sharp shallow-focus
// portraits have the least fine structure of all.
func Junk(s Stats) string {
	switch {
	case s.DarkPct >= JunkDarkPct:
		return "black"
	case s.BrightPct >= JunkBrightPct:
		return "white"
	case s.Contrast < JunkContrast:
		return "uniform"
	}
	return ""
}

// thumbContrast is the luma standard deviation of a 64 px thumbnail: whole-frame
// contrast at the scale a subject stands out from its background.
func thumbContrast(f *Frame) float64 {
	g, _, _, _ := DownLuma(f.Luma, f.W, f.H, 64)
	if len(g) == 0 {
		return 0
	}
	var sum, sq float64
	for _, v := range g {
		sum += float64(v)
		sq += float64(v) * float64(v)
	}
	m := sum / float64(len(g))
	return math.Sqrt(max(sq/float64(len(g))-m*m, 0))
}
