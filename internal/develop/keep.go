package develop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

// Keep is a frame to develop, before its files are read.
type Keep struct {
	Name  string // base name
	Path  string // where it is now
	Model string // EXIF Model when the report knows it; "" = read the DNG
}

// FromReport is the report's keep set: each frame's effective verdict (your label
// over the model's decision, labels.Effective), at the path it has now (keep/ after
// decide --sort). In report order.
func FromReport(results []report.Result, labs map[string]labels.Entry) []Keep {
	var out []Keep
	for _, r := range results {
		if d, _ := labels.Effective(r, labs[filepath.Base(r.File)]); d != eval.Keep {
			continue
		}
		k := Keep{Name: filepath.Base(r.File), Path: r.File}
		if r.MovedTo != "" {
			k.Path = r.MovedTo
		}
		if r.Exif != nil {
			k.Model = r.Exif.Model
		}
		out = append(out, k)
	}
	return out
}

// FromLabels is the keep set of a shoot with labels but no report: the frames you
// labelled keep, found in dir, its sort folders (keep/, review/, cull/), and with
// recursive its other subfolders (never hidden ones). missing lists keeps not found.
// Sorted by name.
func FromLabels(dir string, recursive bool, labs map[string]labels.Entry) (keeps []Keep, missing []string, err error) {
	files, err := pipeline.Discover(dir, recursive)
	if err != nil {
		return nil, nil, err
	}
	for _, sub := range []string{pipeline.KeepDir, pipeline.ReviewDir, pipeline.CullDir, pipeline.CulledDir} {
		more, err := pipeline.Discover(filepath.Join(dir, sub), false)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		files = append(files, more...)
	}
	at := map[string]string{}
	for _, f := range files {
		b := filepath.Base(f)
		if prev, ok := at[b]; ok && prev != f && labs[b].Label == "keep" {
			return nil, nil, fmt.Errorf("two frames are named %s (%s and %s): labels can't say which one you kept", b, prev, f)
		}
		at[b] = f
	}
	for name, e := range labs {
		if e.Label != "keep" {
			continue
		}
		if p, ok := at[name]; ok {
			keeps = append(keeps, Keep{Name: name, Path: p})
		} else {
			missing = append(missing, name)
		}
	}
	sort.Slice(keeps, func(i, j int) bool { return keeps[i].Name < keeps[j].Name })
	sort.Strings(missing)
	return keeps, missing, nil
}

// Load reads what a keep's JPEG is made from: the DNG's size and mtime, its camera
// model (from the DNG when k doesn't carry it), and its sidecar's develop settings.
func Load(k Keep) (Frame, error) {
	st, err := os.Stat(k.Path)
	if err != nil {
		return Frame{}, err
	}
	f := Frame{Name: k.Name, Path: k.Path, Model: k.Model, Size: st.Size(), ModTime: st.ModTime()}
	if f.Model == "" {
		e, err := dng.ReadExif(k.Path)
		if err != nil {
			return Frame{}, fmt.Errorf("%s: reading its camera model: %w", k.Path, err)
		}
		f.Model = e.Model
	}
	sc := xmp.Path(k.Path)
	b, err := os.ReadFile(sc)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return f, nil
	case err != nil:
		return Frame{}, err
	}
	d, err := xmp.ReadDevelop(sc)
	if err != nil {
		return Frame{}, err
	}
	sum := sha256.Sum256(b)
	f.Sidecar, f.SidecarSum, f.EV, f.Crop = sc, hex.EncodeToString(sum[:]), d.ExposureEV, d.Crop != nil
	return f, nil
}
