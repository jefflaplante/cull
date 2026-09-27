package focus

import (
	_ "embed"
	"fmt"
	"image"
	"sort"

	pigo "github.com/esimov/pigo/core"

	"github.com/jefflaplante/gophotocull/internal/imageprep"
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
	Rect image.Rectangle
	Q    float64
	Eyes *image.Point // midpoint between the pupils, when both were found
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
func (d *Detector) Detect(luma []float32, w, h int) (faces []Face) {
	defer func() {
		if recover() != nil {
			faces = nil
		}
	}()
	g, dw, dh, scale := imageprep.DownLuma(luma, w, h, detectEdge)
	img := pigo.ImageParams{Pixels: g, Rows: dh, Cols: dw, Dim: dw}
	cp := pigo.CascadeParams{MinSize: 24, MaxSize: min(dw, dh), ShiftFactor: 0.1, ScaleFactor: 1.1, ImageParams: img}
	var dets []pigo.Detection
	for _, a := range angles {
		dets = append(dets, d.face.RunCascade(cp, a)...)
	}
	dets = d.face.ClusterDetections(dets, 0.2)

	for _, det := range dets {
		half := float64(det.Scale) / 2
		f := Face{
			Rect: image.Rect(
				int((float64(det.Col)-half)/scale), int((float64(det.Row)-half)/scale),
				int((float64(det.Col)+half)/scale), int((float64(det.Row)+half)/scale),
			).Intersect(image.Rect(0, 0, w, h)),
			Q: float64(det.Q),
		}
		if det.Q >= minFaceQ {
			f.Eyes = d.eyes(det, img, scale)
		}
		faces = append(faces, f)
	}
	sort.Slice(faces, func(i, j int) bool { return faces[i].Q > faces[j].Q })
	return faces
}

// eyes runs pigo's pupil localizer with its standard offsets from the face
// centre and returns the midpoint when both pupils are found.
func (d *Detector) eyes(det pigo.Detection, img pigo.ImageParams, scale float64) *image.Point {
	s := float32(det.Scale)
	find := func(sign int) *pigo.Puploc {
		p := d.pup.RunDetector(pigo.Puploc{
			Row: det.Row - int(0.085*s), Col: det.Col + sign*int(0.185*s), Scale: s * 0.4, Perturbs: 63,
		}, img, 0, false)
		if p == nil || p.Row <= 0 || p.Col <= 0 {
			return nil
		}
		return p
	}
	l, r := find(-1), find(1)
	if l == nil || r == nil {
		return nil
	}
	mid := image.Pt(int(float64(l.Col+r.Col)/2/scale), int(float64(l.Row+r.Row)/2/scale))
	return &mid
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
	return Target{Center: c, Size: side, Source: "face"}
}

// BoxTarget covers a model-located box with 20% margin.
func BoxTarget(r image.Rectangle) Target {
	return Target{
		Center: image.Pt((r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2),
		Size:   int(1.2 * float64(max(r.Dx(), r.Dy()))),
		Source: "model",
	}
}
