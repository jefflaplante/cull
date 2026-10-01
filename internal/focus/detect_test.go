package focus

import (
	pigo "github.com/esimov/pigo/core"

	"github.com/jefflaplante/cull/internal/imageprep"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func lumaOf(img image.Image) ([]uint8, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	l := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			l[y*w+x] = uint8((0.299*float32(r) + 0.587*float32(g) + 0.114*float32(bl)) / 257)
		}
	}
	return l, w, h
}

// pigoSample loads pigo's own test portrait from the module cache (never
// committed here); the test is skipped when the module isn't downloaded.
func pigoSample(t *testing.T) image.Image {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/esimov/pigo").Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		t.Skip("pigo module dir not available")
	}
	f, err := os.Open(filepath.Join(dir, "testdata", "sample.jpg"))
	if err != nil {
		t.Skip("pigo sample image not available")
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestDetectFindsFaceInPigoSample(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatal(err)
	}
	l, w, h := lumaOf(pigoSample(t))
	faces := d.Detect(l, w, h)
	if len(faces) == 0 || faces[0].Q < 5 {
		t.Fatalf("no face found in pigo's sample: %+v", faces)
	}
	for i := 1; i < len(faces); i++ {
		if faces[i].Q > faces[i-1].Q {
			t.Fatalf("faces not sorted by Q: %+v", faces)
		}
	}
	if !faces[0].Rect.In(image.Rect(0, 0, w, h)) || faces[0].Rect.Dx() < 20 {
		t.Fatalf("face rect %v not a plausible box inside %dx%d", faces[0].Rect, w, h)
	}
	if len(faces[0].Pupils) != 2 || faces[0].Eyes == nil { // both found before per-angle pupils: must stay so
		t.Fatalf("pupils %v eyes %v", faces[0].Pupils, faces[0].Eyes)
	}
}

func TestOnePupilStillAims(t *testing.T) {
	if p := eyePoint([]image.Point{{100, 50}}); p == nil || *p != (image.Point{100, 50}) {
		t.Fatalf("got %v", p)
	}
	if p := eyePoint([]image.Point{{100, 50}, {140, 54}}); p == nil || *p != (image.Point{120, 52}) {
		t.Fatalf("got %v", p)
	}
	if eyePoint(nil) != nil {
		t.Fatal("no pupils: no eye point")
	}
}

func TestFaceTargetCarriesPupils(t *testing.T) {
	f := Face{Rect: image.Rect(0, 0, 100, 100), Q: 120, Pupils: []image.Point{{30, 40}}, Eyes: &image.Point{30, 40}}
	if tg := FaceTarget(f); len(tg.Pupils) != 1 || tg.Center != (image.Point{30, 40}) {
		t.Fatalf("%+v", tg)
	}
}

func TestDetectFlatFrameHasNoConfidentFace(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatal(err)
	}
	l := make([]uint8, 3000*2000)
	for i := range l {
		l[i] = 120
	}
	if got := Confident(d.Detect(l, 3000, 2000), 80); len(got) != 0 {
		t.Fatalf("flat frame: %+v", got)
	}
}

func TestDetectorIsSafeForConcurrentUse(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatal(err)
	}
	l, w, h := lumaOf(pigoSample(t))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.Detect(l, w, h) }()
	}
	wg.Wait()
}

func TestConfidentAndTargets(t *testing.T) {
	faces := []Face{{Rect: image.Rect(0, 0, 100, 100), Q: 120}, {Rect: image.Rect(0, 0, 50, 50), Q: 40}}
	if got := Confident(faces, 80); len(got) != 1 || got[0].Q != 120 {
		t.Fatalf("Confident: %+v", got)
	}

	eyes := image.Pt(210, 180)
	ft := FaceTarget(Face{Rect: image.Rect(100, 100, 300, 300), Q: 100, Eyes: &eyes})
	if ft.Center != eyes || ft.Size != 200 || ft.Source != "face" {
		t.Fatalf("face with eyes: %+v", ft)
	}
	ft = FaceTarget(Face{Rect: image.Rect(100, 100, 300, 300), Q: 100})
	if ft.Center != image.Pt(200, 180) || ft.Size != 200 { // centre moved up 0.1×side toward the eyes
		t.Fatalf("face without eyes: %+v", ft)
	}

	bt := BoxTarget(image.Rect(1000, 500, 1100, 550))
	if bt.Center != image.Pt(1050, 525) || bt.Size != 120 || bt.Source != "model" {
		t.Fatalf("box target: %+v", bt)
	}
}

// Q is what --face-min-q was calibrated on: clustering across all angles at once
// (pigo sums the Q of what it merges). Finding each face's angle must not change it.
func TestDetectQMatchesAllAngleClustering(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatal(err)
	}
	l, w, h := lumaOf(pigoSample(t))
	g, dw, dh, _ := imageprep.DownLuma(l, w, h, detectEdge)
	img := pigo.ImageParams{Pixels: g, Rows: dh, Cols: dw, Dim: dw}
	cp := pigo.CascadeParams{MinSize: 24, MaxSize: min(dw, dh), ShiftFactor: 0.1, ScaleFactor: 1.1, ImageParams: img}
	var raw []pigo.Detection
	for _, a := range angles {
		raw = append(raw, d.face.RunCascade(cp, a)...)
	}
	best := 0.0
	for _, det := range d.face.ClusterDetections(raw, 0.2) {
		best = max(best, float64(det.Q))
	}
	if faces := d.Detect(l, w, h); len(faces) == 0 || faces[0].Q != best {
		t.Fatalf("top Q %v, all-angle clustering gives %v", faces, best)
	}
}
