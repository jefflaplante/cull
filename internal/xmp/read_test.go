package xmp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDevelop(t *testing.T) {
	ev := func(v float64) *float64 { return &v }
	// An element-form packet, as Lightroom and Camera Raw also write it.
	elements := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
 <rdf:Description rdf:about="" xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/">
  <crs:Exposure2012>-1.25</crs:Exposure2012>
  <crs:HasCrop>True</crs:HasCrop>
  <crs:CropLeft>0.2</crs:CropLeft><crs:CropTop>0.1</crs:CropTop><crs:CropRight>0.8</crs:CropRight><crs:CropBottom>0.9</crs:CropBottom>
 </rdf:Description></rdf:RDF></x:xmpmeta>`
	// Another prefix bound to the crs namespace still counts; the same local name
	// in another namespace doesn't.
	otherPrefix := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
 <rdf:Description rdf:about="" xmlns:cr="http://ns.adobe.com/camera-raw-settings/1.0/" xmlns:foo="urn:foo"
   foo:Exposure2012="3" cr:Exposure2012="+0.40"/></rdf:RDF></x:xmpmeta>`
	cases := []struct {
		name string
		in   string
		want Develop
	}{
		{"cull's own: exposure and crop", string(Render(Sidecar{Label: "Green", ExposureEV: ev(0.5), Crop: &Box{0.1, 0.2, 0.9, 0.8}})),
			Develop{ExposureEV: ev(0.5), Crop: &Box{0.1, 0.2, 0.9, 0.8}}},
		{"cull's own: negative exposure only", string(Render(Sidecar{ExposureEV: ev(-0.3)})), Develop{ExposureEV: ev(-0.3)}},
		{"cull's own: metadata only", string(Render(Sidecar{Rating: 3, Label: "Green", Keywords: []string{"cull:keep"}})), Develop{}},
		{"element form", elements, Develop{ExposureEV: ev(-1.25), Crop: &Box{0.2, 0.1, 0.8, 0.9}}},
		{"other prefix", otherPrefix, Develop{ExposureEV: ev(0.4)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "A.xmp")
			if err := os.WriteFile(p, []byte(c.in), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ReadDevelop(p)
			if err != nil {
				t.Fatal(err)
			}
			if !sameEV(got.ExposureEV, c.want.ExposureEV) || !sameBox(got.Crop, c.want.Crop) {
				t.Errorf("got EV %v crop %v, want EV %v crop %v", deref(got.ExposureEV), got.Crop, deref(c.want.ExposureEV), c.want.Crop)
			}
		})
	}
}

func TestReadDevelopErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadDevelop(filepath.Join(dir, "none.xmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing sidecar: %v, want fs.ErrNotExist", err)
	}
	for name, body := range map[string]string{
		"malformed XML": `<x:xmpmeta><rdf:RDF>`,
		"bad exposure":  `<x:xmpmeta xmlns:x="adobe:ns:meta/"><d xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/" crs:Exposure2012="bright"/></x:xmpmeta>`,
	} {
		p := filepath.Join(dir, name+".xmp")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := ReadDevelop(p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func sameEV(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func sameBox(a, b *Box) bool    { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
