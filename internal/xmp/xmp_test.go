package xmp

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderIsWellFormedXML(t *testing.T) {
	ev := 0.7
	out := Render(Sidecar{Rating: 3, Label: "Green & <ok>", Keywords: []string{"cull:keep"}, ExposureEV: &ev,
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

func TestRenderNamesTheTool(t *testing.T) {
	if s := string(Render(Sidecar{})); !strings.Contains(s, `x:xmptk="cull"`) {
		t.Fatalf("toolkit:\n%s", s)
	}
}

func TestRenderOmitsUnsetRating(t *testing.T) {
	if s := string(Render(Sidecar{Label: "Green"})); strings.Contains(s, "xmp:Rating") || !strings.Contains(s, `xmp:Label="Green"`) {
		t.Fatalf("unrated sidecar:\n%s", s)
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

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "L1.xmp")
	if err := Write(p, Sidecar{Label: "Green"}, false); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, Sidecar{Label: "Red"}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	os.Mkdir(filepath.Join(dir, "L2.xmp"), 0o755) // a destination that can't be replaced
	if err := Write(filepath.Join(dir, "L2.xmp"), Sidecar{}, true); err == nil {
		t.Fatal("want an error writing over a directory")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "Green") {
		t.Fatal("existing sidecar replaced")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestOursRecognisesOnlyCullSidecars(t *testing.T) {
	dir := t.TempDir()
	mine, theirs := filepath.Join(dir, "a.xmp"), filepath.Join(dir, "b.xmp")
	Write(mine, Sidecar{Label: "Green"}, false)
	os.WriteFile(theirs, []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Capture One"/>`), 0o644)
	if !Ours(mine) || Ours(theirs) || Ours(filepath.Join(dir, "none.xmp")) {
		t.Fatalf("mine=%v theirs=%v", Ours(mine), Ours(theirs))
	}
}

func TestRenderHierarchy(t *testing.T) {
	b := Render(Sidecar{Keywords: []string{"cull:keep", "Smith & Jones <2026>"}, Hierarchy: []string{"project|Smith & Jones <2026>", "content|forest"}})
	s := string(b)
	for _, want := range []string{`xmlns:lr="http://ns.adobe.com/lightroom/1.0/"`, "<lr:hierarchicalSubject>", "<rdf:li>project|Smith &amp; Jones &lt;2026&gt;</rdf:li>", "<rdf:li>content|forest</rdf:li>"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if err := xml.Unmarshal(b[strings.Index(s, "<x:xmpmeta"):strings.Index(s, "<?xpacket end")], new(struct{})); err != nil {
		t.Fatalf("not XML: %v", err)
	}
	if strings.Contains(string(Render(Sidecar{Keywords: []string{"a"}})), "hierarchicalSubject") {
		t.Fatal("hierarchy bag written without paths")
	}
}

func TestRenderDateTaken(t *testing.T) {
	out := string(Render(Sidecar{DateTaken: "2025-12-28T00:05:59"}))
	for _, want := range []string{
		`exif:DateTimeOriginal="2025-12-28T00:05:59"`,
		`xmp:CreateDate="2025-12-28T00:05:59"`,
		`photoshop:DateCreated="2025-12-28T00:05:59"`,
		`xmlns:exif="http://ns.adobe.com/exif/1.0/"`,
		`xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if err := xml.Unmarshal([]byte(out[strings.Index(out, "<x:xmpmeta"):strings.Index(out, "<?xpacket end")]), new(any)); err != nil {
		t.Errorf("not well-formed: %v", err)
	}
	plain := string(Render(Sidecar{Rating: 3}))
	for _, no := range []string{"DateTimeOriginal", "CreateDate", "DateCreated", "exif", "photoshop"} {
		if strings.Contains(plain, no) {
			t.Errorf("unexpected %s without DateTaken", no)
		}
	}
}
