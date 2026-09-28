// Package report is the source of truth for a run: downstream appliers (XMP,
// Capture One AppleScript) read from it rather than re-evaluating.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/jefflaplante/gophotocull/internal/dng"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/rawclip"
)

const SchemaVersion = 3

type PreviewInfo struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Orientation int    `json:"orientation"`
	Source      string `json:"source"`
}

// Group places a frame in a burst of near-duplicates.
type Group struct {
	ID   int    `json:"id"`
	Size int    `json:"size"`
	Best string `json:"best"` // base name of the frame kept from the burst
}

// FirstPass is the assessment a frame had before it was escalated to a second model.
type FirstPass struct {
	Backend    string           `json:"backend"`
	Model      string           `json:"model"`
	Evaluation *eval.Evaluation `json:"evaluation"`
}

// FocusTarget records what the sharpness judgement was based on.
type FocusTarget struct {
	Source           string        `json:"source"`                      // face | model | none
	Box              *eval.NormBox `json:"box,omitempty"`               // normalized, display orientation
	FaceQ            float64       `json:"face_q,omitempty"`            // best pigo score, even below --face-min-q
	Faces            int           `json:"faces"`                       // confident faces found
	Label            string        `json:"label,omitempty"`             // model's description of the subject
	Reason           string        `json:"reason,omitempty"`            // why there is no subject crop
	SubjectSharpness float64       `json:"subject_sharpness,omitempty"` // fine/coarse detail ratio of the subject crop
	LandedSharpness  float64       `json:"landed_sharpness"`            // same, best "focus landed" cell
}

type Result struct {
	File        string           `json:"file"`
	Size        int64            `json:"size"`
	ModTime     time.Time        `json:"mod_time"`
	Preview     *PreviewInfo     `json:"preview,omitempty"`
	Exif        *dng.Exif        `json:"exif,omitempty"`
	Stats       *imageprep.Stats `json:"stats,omitempty"`
	FocusTarget *FocusTarget     `json:"focus_target,omitempty"`
	DHash       string           `json:"dhash,omitempty"` // 64-bit difference hash, hex
	RawClip     *rawclip.Result  `json:"raw_clip,omitempty"`
	Group       *Group           `json:"group,omitempty"`
	Evaluation  *eval.Evaluation `json:"evaluation,omitempty"`
	FirstPass   *FirstPass       `json:"first_pass,omitempty"` // set when escalated
	Decision    eval.Decision    `json:"decision,omitempty"`
	Reasons     []string         `json:"reasons,omitempty"`
	Fixups      []string         `json:"fixups,omitempty"`
	Usage       eval.Usage       `json:"usage"`
	CostUSD     float64          `json:"cost_usd,omitempty"` // list price of this frame's calls
	XMP         string           `json:"xmp,omitempty"`
	MovedTo     string           `json:"moved_to,omitempty"` // set by --move-culled; cleared by restore
	Error       string           `json:"error,omitempty"`
}

// Key identifies a file version for resume: same path+size+mtime = already done.
func Key(file string, size int64, mod time.Time) string {
	return fmt.Sprintf("%s|%d|%d", file, size, mod.UnixNano())
}

func (r Result) Key() string { return Key(r.File, r.Size, r.ModTime) }

type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Generated     time.Time `json:"generated"`
	Backend       string    `json:"backend"`
	Escalation    string    `json:"escalation,omitempty"` // "backend/model" frames were escalated to
	Model         string    `json:"model"`
	Dir           string    `json:"dir"`
	Results       []Result  `json:"results"`
}

func Load(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &r, nil
}

// Save writes atomically so an interrupted run never leaves a truncated report.
func (r *Report) Save(path string) error {
	sort.Slice(r.Results, func(i, j int) bool { return r.Results[i].File < r.Results[j].File })
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// WriteCSV writes a flat summary for sorting in a spreadsheet.
func (r *Report) WriteCSV(path string) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"file", "decision", "sharpness", "sharpness_status", "exposure", "exposure_status", "ev_adjust",
		"composition", "composition_status", "crop_ltrb", "reasons",
		"focus_source", "face_q", "subject_sharpness", "landed_sharpness", "error"})
	for _, res := range r.Results {
		row := make([]string, 16)
		row[0], row[1], row[15] = res.File, string(res.Decision), res.Error
		if ft := res.FocusTarget; ft != nil {
			row[11], row[12] = ft.Source, f1(ft.FaceQ)
			row[13] = strconv.FormatFloat(ft.SubjectSharpness, 'f', 3, 64)
			row[14] = strconv.FormatFloat(ft.LandedSharpness, 'f', 3, 64)
		}
		if e := res.Evaluation; e != nil {
			row[2], row[3] = f1(e.Sharpness.Score), e.Sharpness.Status
			row[4], row[5] = f1(e.Exposure.Score), e.Exposure.Status
			row[6] = strconv.FormatFloat(e.Exposure.EVAdjust, 'f', 2, 64)
			row[7], row[8] = f1(e.Composition.Score), e.Composition.Status
			if c := e.Composition.Crop; c.Apply {
				row[9] = fmt.Sprintf("%.3f,%.3f,%.3f,%.3f", c.Left, c.Top, c.Right, c.Bottom)
			}
		}
		if len(res.Reasons) > 0 {
			b, _ := json.Marshal(res.Reasons)
			row[10] = string(b)
		}
		w.Write(row)
	}
	w.Flush()
	return w.Error()
}

// Facts are the measurements the policy combines with a frame's assessment.
func (r Result) Facts() eval.Facts {
	if r.RawClip == nil {
		return eval.Facts{}
	}
	return eval.Facts{RawKnown: true, RawClipPct: r.RawClip.HighlightPct}
}
