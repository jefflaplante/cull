package xmp

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderIsWellFormedXML(t *testing.T) {
	ev := 0.7
	out := Render(Sidecar{Rating: 3, Label: "Green & <ok>", Keywords: []string{"gophotocull:keep"}, ExposureEV: &ev,
		Crop: &Box{0.1, 0.05, 0.9, 0.95}})
	d := xml.NewDecoder(bytesReader(out))
	for {
		if _, err := d.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("malformed XML: %v\n%s", err, out)
		}
	}
}

func TestWriteDoesNotClobber(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.xmp")
	os.WriteFile(p, []byte("existing"), 0o644)
	if err := Write(p, Sidecar{Rating: 1}, false); err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "existing" {
		t.Fatal("sidecar was clobbered")
	}
}

func TestPath(t *testing.T) {
	if got := Path("/x/L1000123.DNG"); got != "/x/L1000123.xmp" {
		t.Fatal(got)
	}
}

func TestFromDisplayRoundTrip(t *testing.T) {
	// A display crop of the top-left quadrant on an orientation-6 image maps to the
	// stored image's bottom-left... verify via corner mapping: display (0,0) = stored (0,1).
	b := FromDisplay(0, 0, 0.5, 0.5, 6)
	if b != (Box{Left: 0, Top: 0.5, Right: 0.5, Bottom: 1}) {
		t.Fatalf("got %+v", b)
	}
	if FromDisplay(0.1, 0.2, 0.3, 0.4, 1) != (Box{0.1, 0.2, 0.3, 0.4}) {
		t.Fatal("identity failed")
	}
}
