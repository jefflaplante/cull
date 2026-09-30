// Package report is the source of truth for a run: downstream appliers (XMP,
// Capture One AppleScript) read from it rather than re-evaluating.
package report

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/rawclip"
)

const SchemaVersion = 4

type PreviewInfo struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Orientation int    `json:"orientation"`
	Source      string `json:"source"`
}

// Group places a frame in a sequence of similar frames (a set).
type Group struct {
	ID       int    `json:"id"`
	Size     int    `json:"size"`               // frames in the set
	Rank     int    `json:"rank"`               // 1 = best; 0 = not ranked
	Of       int    `json:"of"`                 // rankable frames in the set
	By       string `json:"by"`                 // "model" or "scores"
	Best     bool   `json:"best"`               // rank 1..KeepBest (rank 1 when KeepBest is 0)
	Strength string `json:"strength,omitempty"` // the model's note, when ranked by the model
	Weakness string `json:"weakness,omitempty"`
}

// UnmarshalJSON also reads schema-v3 groups, whose "best" was the base name of the
// frame kept from a burst: that becomes false, and the next decide regroups. Any
// other field decodes as usual; Marshal always writes a bool.
func (g *Group) UnmarshalJSON(b []byte) error {
	type plain Group // no methods: no recursion
	aux := struct {
		*plain
		Best json.RawMessage `json:"best"` // shadows plain.Best
	}{plain: (*plain)(g)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	switch {
	case len(aux.Best) == 0:
		return nil
	case aux.Best[0] == '"':
		g.Best = false // v3: a file name
		return nil
	}
	return json.Unmarshal(aux.Best, &g.Best)
}

// Set is one sequence of similar frames. Members (capture order) and Order hold
// Result.File paths. Order is the model's ranking of the members it compared,
// empty if never ranked. The set is ranked only when By is "model": a member the
// model never compared makes it By "scores" while its Order is kept for later.
type Set struct {
	ID      int        `json:"id"`
	Members []string   `json:"members"`
	Of      int        `json:"of"` // rankable members at the last decide
	Order   []string   `json:"order,omitempty"`
	Notes   []RankNote `json:"notes,omitempty"`
	Summary string     `json:"summary,omitempty"`
	By      string     `json:"by"`
	Usage   eval.Usage `json:"usage"`              // the set's rank calls, summed over re-rankings
	CostUSD float64    `json:"cost_usd,omitempty"` // their list price; Report.RankCostUSD holds the total
}

// RankNote is the model's note on one ranked frame.
type RankNote struct {
	File     string `json:"file"`
	Strength string `json:"strength"`
	Weakness string `json:"weakness"`
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
	Look        string           `json:"look,omitempty"` // look fingerprint (imageprep Grid), base64
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
	KeepBest      int       `json:"keep_best"` // Policy.KeepBest used at the last judge or decide
	// Policy is the whole policy the decisions came from (the last judge, decide or
	// rank). decide, rank, calibrate and judge --resume start from it, so tuning
	// survives a later run that doesn't repeat the flags; nil in older reports.
	Policy *eval.Policy `json:"policy,omitempty"`
	// RankCostUSD is the list price of every rank call made for this report. It only
	// grows: a regrouping or re-rank can drop a Set, but not what was paid for it.
	RankCostUSD float64  `json:"rank_cost_usd,omitempty"`
	Results     []Result `json:"results"`
	Sets        []Set    `json:"sets,omitempty"`
}

// EncodeLook stores a look fingerprint compactly.
func EncodeLook(g []uint8) string { return base64.StdEncoding.EncodeToString(g) }

// LookBytes decodes the look fingerprint.
func (r Result) LookBytes() ([]uint8, bool) {
	if r.Look == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(r.Look)
	return b, err == nil && len(b) > 0
}

// PaidWork counts what a report holds that a fresh run would lose: frames with a
// model assessment (paid for) and frames moved into culled/ (only the report
// knows where they came from).
func (r *Report) PaidWork() (evaluated, moved int) {
	for _, x := range r.Results {
		if x.Evaluation != nil {
			evaluated++
		}
		if x.MovedTo != "" {
			moved++
		}
	}
	return evaluated, moved
}

// Cost is everything the report's model calls cost at list price: the frames'
// calls plus every rank call (RankCostUSD; the sets' own CostUSD isn't added again).
func (r *Report) Cost() float64 {
	c := 0.0
	for _, x := range r.Results {
		c += x.CostUSD
	}
	return c + r.RankCostUSD
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
	if r.SchemaVersion < SchemaVersion {
		// Groups from before sequence ranking (schema < 4) carried only a size and a
		// stale "best" file name (see Group.UnmarshalJSON): no current rank. decide and
		// rank always regroup from looks regardless, so drop them here rather than let
		// review or calib show a stale "set 0 · #0/0" badge before that first decide.
		for i := range r.Results {
			r.Results[i].Group = nil
		}
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

// Facts are the measurements the policy combines with a frame's assessment.
func (r Result) Facts() eval.Facts {
	if r.RawClip == nil {
		return eval.Facts{}
	}
	return eval.Facts{RawKnown: true, RawClipPct: r.RawClip.HighlightPct}
}
