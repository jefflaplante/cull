// Package review writes a self-contained HTML contact sheet from a report: every
// frame with its subject crop, the decision and the model's reasoning, and
// keep/review/cull buttons that export a labels CSV for calibration.
package review

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jefflaplante/gophotocull/internal/dng"
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/focus"
	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/report"
)

//go:embed page.html
var pageTemplate string

const (
	thumbEdge   = 1200 // from the smallest embedded preview at least 1500 px
	subjectMax  = 1024 // native-resolution subject crop, capped to keep the sheet light
	placeholder = "/*__DATA__*/null"
)

// Options controls the sheet.
type Options struct {
	Out         string // output directory
	Concurrency int
	Force       bool // regenerate images that already exist
}

type card struct {
	File     string              `json:"file"`
	Path     string              `json:"path"`
	Thumb    string              `json:"thumb,omitempty"`
	Subject  string              `json:"subject,omitempty"`
	Decision string              `json:"decision,omitempty"`
	Reasons  []string            `json:"reasons,omitempty"`
	Fixups   []string            `json:"fixups,omitempty"`
	Error    string              `json:"error,omitempty"`
	Eval     *eval.Evaluation    `json:"eval,omitempty"`
	First    *report.FirstPass   `json:"first,omitempty"`
	Focus    *report.FocusTarget `json:"focus,omitempty"`
	Group    *report.Group       `json:"group,omitempty"`
	Exif     string              `json:"exif,omitempty"`
	MovedTo  string              `json:"moved_to,omitempty"`
	Cost     float64             `json:"cost,omitempty"`
	RawClip  *float64            `json:"raw_clip,omitempty"` // percent of raw samples at white level
}

type pageData struct {
	Title      string `json:"title"`
	Report     string `json:"report"` // labels are stored per report path
	Backend    string `json:"backend,omitempty"`
	Model      string `json:"model,omitempty"`
	Escalation string `json:"escalation,omitempty"`
	Cards      []card `json:"cards"`
}

// Build renders the sheet into o.Out and returns the index.html path.
func Build(rep *report.Report, reportPath string, o Options, log io.Writer) (string, error) {
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return "", err
	}
	cards := make([]card, len(rep.Results))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < max(1, o.Concurrency); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				cards[i] = makeCard(rep, rep.Results[i], o, log)
			}
		}()
	}
	for i := range rep.Results {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	data, err := json.Marshal(pageData{ // Marshal escapes <, >, & so the data can't close its script
		Title: filepath.Base(rep.Dir), Report: reportPath, Backend: rep.Backend, Model: rep.Model,
		Escalation: rep.Escalation, Cards: cards,
	})
	if err != nil {
		return "", err
	}
	page := strings.Replace(pageTemplate, placeholder, string(data), 1)
	index := filepath.Join(o.Out, "index.html")
	return index, os.WriteFile(index, []byte(page), 0o644)
}

func makeCard(rep *report.Report, r report.Result, o Options, log io.Writer) card {
	c := card{
		File: filepath.Base(r.File), Path: r.File, Decision: string(r.Decision), Reasons: r.Reasons,
		Fixups: r.Fixups, Error: r.Error, Eval: r.Evaluation, First: r.FirstPass, Focus: r.FocusTarget,
		Group: r.Group, MovedTo: r.MovedTo, Cost: r.CostUSD,
	}
	if r.Exif != nil {
		c.Exif = r.Exif.Summary()
	}
	if r.RawClip != nil {
		v := r.RawClip.HighlightPct
		c.RawClip = &v
	}
	if r.Error != "" || r.Preview == nil {
		return c
	}
	src := r.File
	if r.MovedTo != "" {
		src = r.MovedTo
	}
	base := baseName(rep.Dir, r.File)
	if name, err := image1(o, base+".thumb.jpg", func() ([]byte, error) { return thumb(src) }); err == nil {
		c.Thumb = name
	} else {
		fmt.Fprintf(log, "%s: thumbnail: %v\n", c.File, err)
	}
	if r.FocusTarget != nil && r.FocusTarget.Box != nil {
		box := *r.FocusTarget.Box
		if name, err := image1(o, base+".subject.jpg", func() ([]byte, error) { return subject(src, box) }); err == nil {
			c.Subject = name
		} else {
			fmt.Fprintf(log, "%s: subject crop: %v\n", c.File, err)
		}
	}
	return c
}

// image1 writes one image unless it already exists (and !Force), returning its
// file name relative to the sheet.
func image1(o Options, name string, render func() ([]byte, error)) (string, error) {
	p := filepath.Join(o.Out, name)
	if !o.Force {
		if _, err := os.Stat(p); err == nil {
			return name, nil
		}
	}
	b, err := render()
	if err != nil {
		return "", err
	}
	return name, os.WriteFile(p, b, 0o644)
}

func thumb(path string) ([]byte, error) {
	pv, err := dng.PreviewAtLeast(path, 1500)
	if err != nil {
		return nil, err
	}
	f, err := imageprep.Decode(pv.Data, pv.Orientation)
	if err != nil {
		return nil, err
	}
	return f.Downscaled(thumbEdge, 82)
}

// subject crops the recorded focus-target box at native resolution, as the model
// saw it, capped at subjectMax pixels around its centre.
func subject(path string, box eval.NormBox) ([]byte, error) {
	pv, err := dng.Extract(path)
	if err != nil {
		return nil, err
	}
	f, err := imageprep.Decode(pv.Data, pv.Orientation)
	if err != nil {
		return nil, err
	}
	r := image.Rect(int(box.Left*float64(f.W)), int(box.Top*float64(f.H)), int(box.Right*float64(f.W)), int(box.Bottom*float64(f.H)))
	t := focus.BoxTarget(r)
	crop := focus.SubjectRect(t, f.W, f.H)
	if crop.Dx() > subjectMax {
		c := image.Pt((crop.Min.X+crop.Max.X)/2, (crop.Min.Y+crop.Max.Y)/2)
		crop = image.Rect(c.X-subjectMax/2, c.Y-subjectMax/2, c.X+subjectMax/2, c.Y+subjectMax/2)
	}
	return f.Crop(crop, 85)
}

// baseName names a frame's images by its path relative to the shoot, so frames
// with the same name in different folders don't collide.
func baseName(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	return strings.ReplaceAll(rel, string(filepath.Separator), "__")
}
