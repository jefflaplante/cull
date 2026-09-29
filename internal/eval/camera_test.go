package eval

import (
	"strings"
	"testing"
)

// The prompts describe the camera from each frame's EXIF instead of assuming a Leica
// M11-P: an autofocus body or a phone must not be told it is a manual-focus
// rangefinder, and a Leica M still is.
func TestCameraDescribe(t *testing.T) {
	for _, c := range []struct {
		cam  Camera
		want string
	}{
		{Camera{Make: "Leica Camera AG", Model: "LEICA M11-P"}, "a LEICA M11-P rangefinder (manual focus, often fast lenses shot wide open)"},
		{Camera{Make: "Leica Camera AG", Model: "LEICA M10-R"}, "a LEICA M10-R rangefinder (manual focus, often fast lenses shot wide open)"},
		{Camera{Make: "Leica Camera AG", Model: "LEICA M MONOCHROM (Typ 246)"}, "a LEICA M MONOCHROM (Typ 246) rangefinder (manual focus, often fast lenses shot wide open)"},
		{Camera{Make: "LEICA CAMERA AG", Model: "LEICA SL2"}, "a LEICA SL2"},
		{Camera{Make: "Leica Camera AG", Model: "LEICA Q2"}, "a LEICA Q2"},
		{Camera{Make: "Canon", Model: "Canon EOS 5D Mark III"}, "a Canon EOS 5D Mark III"},
		{Camera{Make: "Apple", Model: "iPhone 12 Pro"}, "an Apple iPhone 12 Pro"},
		{Camera{Make: "RICOH IMAGING COMPANY, LTD.", Model: "RICOH GR III"}, "a RICOH GR III"},
		{Camera{Make: "RICOH IMAGING COMPANY, LTD.", Model: "PENTAX K-1 Mark II"}, "a RICOH PENTAX K-1 Mark II"},
		{Camera{Make: "OLYMPUS IMAGING CORP.", Model: "E-M1"}, "an OLYMPUS E-M1"},
		{Camera{Make: "SIGMA", Model: ""}, "a SIGMA camera"},
		{Camera{Make: "NIKON CORPORATION", Model: ""}, "a NIKON camera"},
		{Camera{}, "a digital camera"},
	} {
		if got := c.cam.Describe(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.cam, got, c.want)
		}
	}
}

func TestPromptsNameTheFramesCamera(t *testing.T) {
	canon := Camera{Make: "Canon", Model: "Canon EOS 5D Mark III"}
	m11 := Camera{Make: "Leica Camera AG", Model: "LEICA M11-P"}
	for name, sys := range map[string]string{
		"evaluate": SystemPrompt(0.6, canon),
		"locate":   LocateRequest([]byte{0xFF, 0xD8}, canon, 256).System,
	} {
		if !strings.Contains(sys, "Canon EOS 5D Mark III") {
			t.Errorf("%s prompt doesn't name the camera:\n%s", name, sys)
		}
		for _, bad := range []string{"Leica", "LEICA", "M11", "rangefinder", "manual focus"} {
			if strings.Contains(sys, bad) {
				t.Errorf("%s prompt for a Canon mentions %q", name, bad)
			}
		}
	}
	for name, sys := range map[string]string{
		"evaluate": SystemPrompt(0.6, m11),
		"locate":   LocateRequest([]byte{0xFF, 0xD8}, m11, 256).System,
	} {
		if !strings.Contains(sys, "LEICA M11-P rangefinder (manual focus") {
			t.Errorf("%s prompt lost the rangefinder description:\n%s", name, sys)
		}
	}
	if !strings.Contains(EvalRequest(Input{Camera: canon, MinCropArea: 0.6}).System, "Canon EOS 5D Mark III") {
		t.Error("EvalRequest doesn't pass the frame's camera to the prompt")
	}
}
