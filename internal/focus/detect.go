package focus

import (
	_ "embed"
	"fmt"
	"image"
	"sort"

	pigo "github.com/esimov/pigo/core"

	"github.com/jefflaplante/cull/internal/imageprep"
)

//go:embed cascade/facefinder
var facefinderCascade []byte

//go:embed cascade/puploc
var puplocCascade []byte

// Detection parameters measured on real M11-P frames (CLAUDE.md, "pigo").
const (
	detectEdge = 2000
	minFaceQ   = 5 // pigo's usual floor for running the pupil localizer
)

// angles are fractions of a full turn: 0°, +18°, -18° (tilted heads).
var angles = []float64{0, 0.05, 0.95}

// Face is a detection in display-oriented native pixels.
type Face struct {
	Rect   image.Rectangle
	Q      float64
	Angle  float64       // the cascade angle it was found at (fraction of a turn)
	Pupils []image.Point // pupils found (0-2), native pixels
	Eyes   *image.Point  // aim point: between both pupils, or the one found
}

// Detector holds the unpacked cascades. They are read-only after NewDetector,
// so one Detector can serve every worker.
type Detector struct {
	face *pigo.Pigo
	pup  *pigo.PuplocCascade
}

func NewDetector() (*Detector, error) {
	face, err := pigo.NewPigo().Unpack(facefinderCascade)
	if err != nil {
		return nil, fmt.Errorf("facefinder cascade: %w", err)
	}
	pup, err := pigo.NewPuplocCascade().UnpackCascade(puplocCascade)
	if err != nil {
		return nil, fmt.Errorf("puploc cascade: %w", err)
	}
	return &Detector{face: face, pup: pup}, nil
}

// Detect returns clustered face detections sorted by Q, highest first. A panic
// inside pigo is treated as "no face": detection is advisory, never fatal.
func (d *Detector) Detect(luma []uint8, w, h int) (faces []Face) {
	defer func() {
		if recover() != nil {
			faces = nil
		}
	}()
	g, dw, dh, scale := imageprep.DownLuma(luma, w, h, detectEdge)
	img := pigo.ImageParams{Pixels: g, Rows: dh, Cols: dw, Dim: dw}
	cp := pigo.CascadeParams{MinSize: 24, MaxSize: min(dw, dh), ShiftFactor: 0.1, ScaleFactor: 1.1, ImageParams: img}
	// Cluster across all angles at once, as --face-min-q was calibrated on (pigo
	// sums the Q of what it merges). Each face's angle is the one whose raw
	// detections contributed most to it: the pupil localizer must look at that
	// angle too.
	type angled struct {
		det   pigo.Detection
		angle float64
	}
	var raw []angled
	var plain []pigo.Detection
	for _, a := range angles {
		for _, det := range d.face.RunCascade(cp, a) {
			raw = append(raw, angled{det, a})
			plain = append(plain, det)
		}
	}
	var dets []angled
	for _, det := range d.face.ClusterDetections(plain, 0.2) {
		weight := map[float64]float32{}
		for _, r := range raw {
			if iou(r.det, det) > 0.2 {
				weight[r.angle] += r.det.Q
			}
		}
		best := angles[0]
		for _, a := range angles {
			if weight[a] > weight[best] {
				best = a
			}
		}
		dets = append(dets, angled{det, best})
	}

	for _, ad := range dets {
		det := ad.det
		half := float64(det.Scale) / 2
		f := Face{
			Rect: image.Rect(
				int((float64(det.Col)-half)/scale), int((float64(det.Row)-half)/scale),
				int((float64(det.Col)+half)/scale), int((float64(det.Row)+half)/scale),
			).Intersect(image.Rect(0, 0, w, h)),
			Q:     float64(det.Q),
			Angle: ad.angle,
		}
		if det.Q >= minFaceQ {
			// Upright first, as before (on real frames its aim is well centred); a
			// tilted face that showed fewer than both pupils gets a second look at
			// its own angle.
			f.Pupils = d.pupils(det, img, scale, 0)
			if len(f.Pupils) < 2 && ad.angle != 0 {
				if alt := d.pupils(det, img, scale, ad.angle); len(alt) > len(f.Pupils) {
					f.Pupils = alt
				}
			}
			f.Eyes = eyePoint(f.Pupils)
		}
		faces = append(faces, f)
	}
	sort.Slice(faces, func(i, j int) bool { return faces[i].Q > faces[j].Q })
	return faces
}

// pupils runs pigo's pupil localizer with its standard offsets from the face
// centre, at the angle the face was found at, and returns the pupils it found
// (0-2) in native pixels.
func (d *Detector) pupils(det pigo.Detection, img pigo.ImageParams, scale, angle float64) []image.Point {
	s := float32(det.Scale)
	var out []image.Point
	for _, sign := range []int{-1, 1} {
		p := d.pup.RunDetector(pigo.Puploc{
			Row: det.Row - int(0.085*s), Col: det.Col + sign*int(0.185*s), Scale: s * 0.4, Perturbs: 63,
		}, img, angle, false)
		if p == nil || p.Row <= 0 || p.Col <= 0 {
			continue
		}
		out = append(out, image.Pt(int(float64(p.Col)/scale), int(float64(p.Row)/scale)))
	}
	return out
}

// eyePoint aims between both pupils, or at the one found: a turned or
// half-shadowed face often shows one eye, and that eye is where focus belongs.
func eyePoint(pupils []image.Point) *image.Point {
	switch len(pupils) {
	case 0:
		return nil
	case 1:
		p := pupils[0]
		return &p
	}
	mid := image.Pt((pupils[0].X+pupils[1].X)/2, (pupils[0].Y+pupils[1].Y)/2)
	return &mid
}

// iou is the overlap of two detections' square boxes.
func iou(a, b pigo.Detection) float64 {
	box := func(d pigo.Detection) image.Rectangle {
		h := d.Scale / 2
		return image.Rect(d.Col-h, d.Row-h, d.Col+h, d.Row+h)
	}
	ra, rb := box(a), box(b)
	in := ra.Intersect(rb)
	if in.Empty() {
		return 0
	}
	area := func(r image.Rectangle) float64 { return float64(r.Dx() * r.Dy()) }
	return area(in) / (area(ra) + area(rb) - area(in))
}

// Confident keeps faces scoring at least minQ, preserving order.
func Confident(faces []Face, minQ float64) []Face {
	var out []Face
	for _, f := range faces {
		if f.Q >= minQ {
			out = append(out, f)
		}
	}
	return out
}

// FaceTarget aims at the eyes when found, else a little above the face centre,
// where the eyes usually are.
func FaceTarget(f Face) Target {
	side := max(f.Rect.Dx(), f.Rect.Dy())
	c := image.Pt((f.Rect.Min.X+f.Rect.Max.X)/2, (f.Rect.Min.Y+f.Rect.Max.Y)/2-side/10)
	if f.Eyes != nil {
		c = *f.Eyes
	}
	return Target{Center: c, Size: side, Source: "face", Pupils: f.Pupils}
}

// BoxTarget covers a model-located box with 20% margin.
func BoxTarget(r image.Rectangle) Target {
	return Target{
		Center: image.Pt((r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2),
		Size:   int(1.2 * float64(max(r.Dx(), r.Dy()))),
		Source: "model",
	}
}
