package xmp

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// crsNS is the Camera Raw settings namespace (crs:), whatever prefix a packet binds it to.
const crsNS = "http://ns.adobe.com/camera-raw-settings/1.0/"

// Develop is the develop settings a sidecar carries: what cull develop hands on to
// LightCraft and must re-assert after its auto stages. Nil fields are absent.
type Develop struct {
	ExposureEV *float64 // crs:Exposure2012
	Crop       *Box     // crs:Crop*, when crs:HasCrop is True
}

// ReadDevelop reads the crs: develop settings from the sidecar at path, in attribute
// or element form (Render writes attributes; Lightroom writes either). A missing
// sidecar is an error wrapping fs.ErrNotExist.
func ReadDevelop(path string) (Develop, error) {
	f, err := os.Open(path)
	if err != nil {
		return Develop{}, err
	}
	defer f.Close()
	vals := map[string]string{}
	d := xml.NewDecoder(f)
	var inCRS string // the crs: element whose text is being read
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Develop{}, fmt.Errorf("%s: %w", path, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			for _, a := range t.Attr {
				if a.Name.Space == crsNS {
					vals[a.Name.Local] = a.Value
				}
			}
			inCRS = ""
			if t.Name.Space == crsNS {
				inCRS = t.Name.Local
			}
		case xml.CharData:
			if inCRS != "" {
				vals[inCRS] += string(t)
			}
		case xml.EndElement:
			inCRS = ""
		}
	}
	var dv Develop
	num := func(k string) (float64, bool, error) {
		s, ok := vals[k]
		if !ok {
			return 0, false, nil
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s: crs:%s %q: %w", path, k, s, err)
		}
		return v, true, nil
	}
	if v, ok, err := num("Exposure2012"); err != nil {
		return Develop{}, err
	} else if ok {
		dv.ExposureEV = &v
	}
	if strings.EqualFold(strings.TrimSpace(vals["HasCrop"]), "true") {
		var b Box
		for _, e := range []struct {
			k string
			v *float64
		}{{"CropLeft", &b.Left}, {"CropTop", &b.Top}, {"CropRight", &b.Right}, {"CropBottom", &b.Bottom}} {
			v, ok, err := num(e.k)
			if err != nil {
				return Develop{}, err
			}
			if !ok {
				return Develop{}, fmt.Errorf("%s: crs:HasCrop without crs:%s", path, e.k)
			}
			*e.v = v
		}
		dv.Crop = &b
	}
	return dv, nil
}
