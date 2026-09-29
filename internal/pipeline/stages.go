package pipeline

import (
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/rawclip"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

// The stages of one frame, shared by the synchronous pipeline and batch mode:
// prepareFrame → faceTarget → (locate) → applyLocate → buildInput → (evaluate) → finish.

// prepared is a measured, decoded frame ready for model inputs.
type prepared struct {
	res         report.Result
	frame       *imageprep.Frame
	stats       imageprep.Stats
	orientation int
}

// prepareFrame reads the preview, EXIF and raw clipping and decodes the frame. On
// error the result already records the failure.
func prepareFrame(cfg Config, path string) (*prepared, error) {
	p := &prepared{res: report.Result{File: path}}
	res := &p.res
	if st, err := os.Stat(path); err == nil {
		res.Size, res.ModTime = st.Size(), st.ModTime()
	}
	pv, err := dng.Best(path, cfg.MinPreviewEdge)
	if err != nil {
		res.Error = "preview: " + err.Error()
		return p, err
	}
	p.orientation = pv.Orientation
	res.Preview = &report.PreviewInfo{Width: pv.Width, Height: pv.Height, Orientation: pv.Orientation, Source: pv.Source}
	if ex, err := dng.ReadExif(path); err == nil {
		res.Exif = &ex
	}
	p.frame, err = imageprep.Decode(pv.Data, pv.Orientation)
	if err != nil {
		res.Error = "decode: " + err.Error()
		return p, err
	}
	p.stats = imageprep.Measure(p.frame)
	res.Stats = &p.stats
	res.Look = report.EncodeLook(p.frame.Grid(group.LookSize))
	if cfg.RawClip {
		if rc, err := rawclip.Measure(path); err == nil {
			res.RawClip = rc
		} else {
			res.Fixups = append(res.Fixups, "raw clip: "+err.Error())
		}
	}
	if pv.LongEdge() < cfg.MinPreviewEdge {
		res.Fixups = append(res.Fixups, fmt.Sprintf("preview long edge %dpx < %dpx: focus judgement unreliable", pv.LongEdge(), cfg.MinPreviewEdge))
	}
	res.FocusTarget = &report.FocusTarget{}
	return p, nil
}

// faceTarget returns the most confident face as the target, or reports whether
// the model should be asked to locate one.
func faceTarget(cfg Config, frame *imageprep.Frame, ft *report.FocusTarget) (*focus.Target, bool) {
	all := cfg.detect(frame)
	if len(all) > 0 {
		ft.FaceQ = round(all[0].Q, 1) // recorded even below --face-min-q, for calibrating it
	}
	faces := focus.Confident(all, cfg.FaceMinQ)
	ft.Faces = len(faces)
	if len(faces) > 0 {
		t := focus.FaceTarget(faces[0])
		ft.Source, ft.Box = "face", normBox(faces[0].Rect, frame.W, frame.H)
		return &t, false
	}
	ft.Source = "none"
	switch {
	case cfg.DryRun:
		ft.Reason = "no face; scan does not call a model"
		return nil, false
	case !cfg.Locate:
		ft.Reason = "no face; locate off"
		return nil, false
	}
	return nil, true
}

// applyLocate records a locate answer (or its failure) and returns the target.
// A failed or unusable answer only means "no subject crop".
func applyLocate(ft *report.FocusTarget, loc *eval.LocateResult, err error, frame *imageprep.Frame) *focus.Target {
	switch {
	case err != nil && loc == nil:
		ft.Reason = "locate failed: " + err.Error()
	case loc == nil:
		ft.Reason = "locate failed: no result"
	case !loc.Confident || loc.Kind == "none":
		ft.Reason = "model: no clear subject"
	case !loc.Box.Valid():
		ft.Reason = fmt.Sprintf("model: invalid box %+v", loc.Box)
	default:
		box := loc.Box
		t := focus.BoxTarget(denorm(box, frame.W, frame.H))
		ft.Source, ft.Box, ft.Label, ft.Reason = "model", &box, loc.Subject, ""
		return &t
	}
	return nil
}

// buildInput makes the subject crop, "focus landed" tiles and full frame, saves
// them when --save-inputs is set, and returns the evaluation input. In a dry run
// without --save-inputs it stops after the measurements.
func buildInput(cfg Config, p *prepared, target *focus.Target) (eval.Input, error) {
	res, frame, ft := &p.res, p.frame, p.res.FocusTarget
	var subjectRect image.Rectangle
	if target != nil {
		subjectRect = focus.SubjectRect(*target, frame.W, frame.H)
	}
	cells, noise := focus.Landed(frame.Luma, frame.W, frame.H, subjectRect, cfg.LandedTiles)
	if len(cells) > 0 {
		ft.LandedSharpness = round(cells[0].Ratio, 3)
	}
	if target != nil && !cfg.LandedWithSubject {
		cells = nil // measured for the report, but not sent: see Config.LandedWithSubject
	}
	var subject *eval.Labeled
	if target != nil {
		ft.SubjectSharpness = round(focus.Ratio(frame.Luma, frame.W, subjectRect, noise), 3)
		jb, err := frame.Crop(subjectRect, 90)
		if err != nil {
			return eval.Input{}, fmt.Errorf("crop: %w", err)
		}
		subject = &eval.Labeled{Label: subjectLabel(ft), JPEG: jb}
	}
	var landed []eval.Labeled
	for _, c := range cells {
		jb, err := frame.Crop(c.Crop, 90)
		if err != nil {
			return eval.Input{}, fmt.Errorf("crop: %w", err)
		}
		landed = append(landed, eval.Labeled{Label: landedLabel, JPEG: jb})
	}
	if cfg.DryRun && cfg.SaveInputs == "" {
		return eval.Input{}, nil
	}
	full, err := frame.Downscaled(cfg.Prep.MaxEdge, 85)
	if err != nil {
		return eval.Input{}, fmt.Errorf("encode: %w", err)
	}
	in := eval.Input{
		Filename:    filepath.Base(res.File),
		FullFrame:   full,
		Subject:     subject,
		Landed:      landed,
		StatsText:   statsText(frame, p.stats, res.Exif, res.RawClip),
		MinCropArea: cfg.Policy.MinCropArea,
		Camera:      cameraOf(res.Exif),
	}
	if cfg.SaveInputs != "" {
		names := []string{"full"}
		if subject != nil {
			names = append(names, "subject")
		}
		for i := range landed {
			names = append(names, fmt.Sprintf("landed-%d", i+1))
		}
		if err := saveInputs(cfg.SaveInputs, inputsBase(cfg.Dir, res.File), eval.EvalRequest(in), names, ft); err != nil {
			res.Fixups = append(res.Fixups, "save-inputs: "+err.Error())
		}
	}
	return in, nil
}

// finish applies the policy to an assessment and writes the sidecar.
func finish(cfg Config, res *report.Result, e *eval.Evaluation, orientation int) {
	res.Fixups = append(res.Fixups, cfg.Policy.Sanitize(e)...)
	res.Evaluation = e
	res.Decision, res.Reasons = cfg.Policy.DecideFacts(e, res.Facts())
	if !cfg.WriteXMP {
		return
	}
	p := xmp.Path(res.File)
	switch err := xmp.Write(p, labels.Sidecar(*res, labels.Entry{}, orientation, cfg.XMPDevelop), cfg.OverwriteXMP); {
	case err == nil:
		res.XMP = p
	case errors.Is(err, xmp.ErrExists):
		res.Fixups = append(res.Fixups, "xmp: sidecar exists, not overwritten")
	default:
		res.Fixups = append(res.Fixups, "xmp: "+err.Error())
	}
}

// cameraOf names a frame's camera for the prompts; a frame without EXIF gets a
// generic description.
func cameraOf(e *dng.Exif) eval.Camera {
	if e == nil {
		return eval.Camera{}
	}
	return eval.Camera{Make: e.Make, Model: e.Model}
}
