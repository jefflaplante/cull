package review

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/report"
)

// The sheet's images are a cache in its assets folder. Each is named after what it
// was made from: the DNG's size and modification time, the focus box for a subject
// crop, and assetVersion. An image whose name exists is current, so a build renders
// only what's missing; a re-judged frame or a changed file gets a new name, and
// the stale image is removed. Sorting into keep/ review/ cull/ renames the DNG
// without changing its size or time, so it keeps its images.

// assetVersion is part of every image's name: bump it when a renderer changes, and
// the next build replaces the older images.
const assetVersion = 1

func assetName(base, kind string, r report.Result, extra string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("v%d|%d|%d|%s", assetVersion, r.Size, r.ModTime.UnixNano(), extra)))
	return fmt.Sprintf("%s.%s.%x.jpg", base, kind, h[:4])
}

func thumbName(base string, r report.Result) string { return assetName(base, "thumb", r, "") }

func subjectName(base string, r report.Result, b eval.NormBox) string {
	return assetName(base, "subject", r, fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", b.Left, b.Top, b.Right, b.Bottom))
}

func nativeName(base string, r report.Result) string { return assetName(base, "native", r, "") }

// CachedImages are the names, in the assets folder, of the images a sheet keeps for r
// (thumbnail, subject crop when r has a focus box, the loupe's native image), with
// root the shoot folder: built from r.File's name, size and modification time. cull
// rename carries them from the old name's to the new one's.
func CachedImages(root string, r report.Result) []string {
	base := baseName(root, r.File)
	out := []string{thumbName(base, r), nativeName(base, r)}
	if r.FocusTarget != nil && r.FocusTarget.Box != nil {
		out = append(out, subjectName(base, r, *r.FocusTarget.Box))
	}
	return out
}

// ours reports whether a file in the assets folder is one of the sheet's cached
// images (current, stale, from before cache names, or a partial write).
func ours(name string) bool {
	if !strings.HasSuffix(name, ".jpg") && !strings.HasSuffix(name, ".jpg.tmp") {
		return false
	}
	for _, k := range []string{".thumb.", ".subject.", ".native."} {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}

// writeAsset writes an image atomically: a reader (the review server, a second build)
// never sees half of one.
func writeAsset(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Prerender writes r's thumbnail and subject crop into out's assets folder unless
// current ones are there. f is the frame's full preview, already decoded (judge and
// scan have it in hand): the subject crop is cut from it, which saves review a full
// decode per frame. root is the shoot folder images are named relative to.
func Prerender(out, root string, r report.Result, f *imageprep.Frame) error {
	dir := filepath.Join(out, AssetsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := baseName(root, r.File)
	var errs []error
	if p := filepath.Join(dir, thumbName(base, r)); !exists(p) {
		b, err := thumb(r.File)
		if err == nil {
			err = writeAsset(p, b)
		}
		errs = append(errs, err)
	}
	if r.FocusTarget != nil && r.FocusTarget.Box != nil {
		box := *r.FocusTarget.Box
		if p := filepath.Join(dir, subjectName(base, r, box)); !exists(p) {
			b, err := subjectFrom(f, box)
			if err == nil {
				err = writeAsset(p, b)
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweep removes the cached images in out's assets folder that no card shows: stale
// versions and frames no longer in the report. It returns how many it removed.
func sweep(out string, keep map[string]bool) int {
	dir := filepath.Join(out, AssetsDir)
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if ours(e.Name()) && !keep[e.Name()] && os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// ClearCache removes every cached image from out's assets folder (nothing else in
// it), and returns how many it removed. The next build renders them again.
func ClearCache(out string) (int, error) {
	dir := filepath.Join(out, AssetsDir)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if !ours(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
