// Package xmp writes XMP sidecars.
//
// Consumer compatibility (verify with one file before trusting at scale):
//   - xmp:Rating, xmp:Label, dc:subject keywords: read by Capture One (Sync Metadata)
//     and Lightroom.
//   - crs:* develop settings (exposure, crop): Adobe Camera Raw semantics. Capture One
//     does not reliably translate these from sidecars; use the Capture One AppleScript
//     applier for edits there. Lightroom Classic reads XMP embedded in DNGs and
//     ignores sidecars for DNG files.
package xmp

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrExists = errors.New("sidecar already exists")

// Sidecar is what gets written. Nil/zero optional fields are omitted.
type Sidecar struct {
	Rating     int      // 1-5; 0 omits xmp:Rating
	Label      string   // e.g. "Red"; empty to omit
	Keywords   []string // dc:subject: plain keywords
	Hierarchy  []string // lr:hierarchicalSubject: "parent|child" paths (Capture One and Lightroom nest them)
	ExposureEV *float64 // crs:Exposure2012
	Crop       *Box     // crs:Crop*, normalized, in the raw's stored (unrotated) orientation
}

type Box struct{ Left, Top, Right, Bottom float64 }

// Path returns the conventional sidecar path: basename with .xmp replacing the
// extension (L1000123.DNG -> L1000123.xmp), which is what Capture One and Adobe use.
func Path(imagePath string) string {
	return strings.TrimSuffix(imagePath, filepath.Ext(imagePath)) + ".xmp"
}

// marker is the toolkit attribute Render writes: a sidecar still carrying it was
// written by cull and not rewritten since (Capture One or Adobe would replace it).
const marker = `x:xmptk="cull"`

// Ours reports whether the sidecar at path was written by cull, so a sidecar whose
// ownership record was lost (a crash before the report's checkpoint) is still
// recognised, while one another tool has rewritten is not.
func Ours(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4096) // the marker is in the packet's first lines
	n, _ := io.ReadFull(f, b)
	return bytes.Contains(b[:n], []byte(marker))
}

// Write renders and writes the sidecar atomically: a unique temp file beside it,
// synced, then linked into place (link(2) fails if the name exists, so an existing
// sidecar, possibly holding someone's real edits, is never replaced unless
// overwrite, even by a concurrent writer) or renamed over it when overwriting.
// Filesystems without hard links fall back to check-then-rename. The temp file
// never outlives the call.
func Write(path string, s Sidecar, overwrite bool) error {
	if !overwrite {
		if _, err := os.Lstat(path); err == nil {
			return ErrExists
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after a link this drops the spare name; after a rename, nothing is left to remove
	if _, err := f.Write(Render(s)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp makes 0600. Best effort: some network volumes refuse mode
	// changes, and the mode is not worth failing a sidecar over.
	_ = os.Chmod(tmp, 0o644)
	if overwrite {
		return os.Rename(tmp, path)
	}
	switch err := os.Link(tmp, path); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrExists
	}
	return os.Rename(tmp, path)
}

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Render produces the XMP packet.
func Render(s Sidecar) []byte {
	var attrs []string
	if s.Rating > 0 {
		attrs = append(attrs, fmt.Sprintf(`xmp:Rating="%d"`, s.Rating))
	}
	if s.Label != "" {
		attrs = append(attrs, fmt.Sprintf(`xmp:Label="%s"`, esc(s.Label)))
	}
	if s.ExposureEV != nil || s.Crop != nil {
		attrs = append(attrs, `crs:HasSettings="True"`)
	}
	if s.ExposureEV != nil {
		attrs = append(attrs, fmt.Sprintf(`crs:Exposure2012="%+.2f"`, *s.ExposureEV))
	}
	if s.Crop != nil {
		attrs = append(attrs,
			`crs:HasCrop="True"`,
			fmt.Sprintf(`crs:CropLeft="%.6f"`, s.Crop.Left),
			fmt.Sprintf(`crs:CropTop="%.6f"`, s.Crop.Top),
			fmt.Sprintf(`crs:CropRight="%.6f"`, s.Crop.Right),
			fmt.Sprintf(`crs:CropBottom="%.6f"`, s.Crop.Bottom),
			`crs:CropAngle="0"`,
		)
	}
	bag := func(tag string, items []string) string {
		if len(items) == 0 {
			return ""
		}
		var li strings.Builder
		for _, k := range items {
			fmt.Fprintf(&li, "\n     <rdf:li>%s</rdf:li>", esc(k))
		}
		return fmt.Sprintf("\n   <%s>\n    <rdf:Bag>%s\n    </rdf:Bag>\n   </%s>", tag, li.String(), tag)
	}
	kw := bag("dc:subject", s.Keywords) + bag("lr:hierarchicalSubject", s.Hierarchy)
	if kw != "" {
		kw += "\n  "
	}
	body := "/>"
	if kw != "" {
		body = ">" + kw + "</rdf:Description>"
	}
	return []byte(fmt.Sprintf(`<?xpacket begin="`+"\ufeff"+`" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="cull">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:xmp="http://ns.adobe.com/xap/1.0/"
    xmlns:dc="http://purl.org/dc/elements/1.1/"
    xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/"
    xmlns:lr="http://ns.adobe.com/lightroom/1.0/"
    %s%s
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>
`, strings.Join(attrs, "\n    "), body))
}

// FromDisplay converts a normalized crop in displayed (EXIF-oriented) coordinates to
// the stored orientation. ASSUMPTION TO VERIFY: crs:Crop* are expressed in the raw's
// stored orientation. Test with one portrait-orientation file in ACR before relying on it.
func FromDisplay(l, t, r, b float64, orientation int) Box {
	switch orientation {
	case 3:
		return Box{Left: 1 - r, Top: 1 - b, Right: 1 - l, Bottom: 1 - t}
	case 6: // display = stored rotated 90° CW
		return Box{Left: t, Top: 1 - r, Right: b, Bottom: 1 - l}
	case 8: // display = stored rotated 90° CCW
		return Box{Left: 1 - b, Top: l, Right: 1 - t, Bottom: r}
	default:
		return Box{Left: l, Top: t, Right: r, Bottom: b}
	}
}
