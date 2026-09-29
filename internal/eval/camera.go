package eval

import (
	"regexp"
	"strings"
)

// Camera is the body a frame came from, as its EXIF names it. The prompts describe
// it so the model isn't told every file is from one particular camera.
type Camera struct {
	Make  string
	Model string
}

// leicaM matches Leica M bodies: manual-focus rangefinders, where missed focus is a
// common failure and fast lenses are often shot wide open.
var leicaM = regexp.MustCompile(`(?i)^leica m(\d|[\s-]|$)`)

// Describe names the camera for a prompt, with an article: "a Canon EOS 5D Mark III",
// "an Apple iPhone 12 Pro", "a LEICA M11-P rangefinder (manual focus, ...)", or "a
// digital camera" when EXIF names nothing.
func (c Camera) Describe() string {
	mk, model := strings.TrimSpace(c.Make), strings.TrimSpace(c.Model)
	var name string
	switch {
	case mk == "" && model == "":
		return "a digital camera"
	case model == "":
		name = firstWord(mk) + " camera"
	case mk == "" || strings.EqualFold(firstWord(mk), firstWord(model)):
		name = model // "Canon EOS 5D Mark III", not "Canon Canon EOS 5D Mark III"
	default:
		// The brand only: "RICOH IMAGING COMPANY, LTD." + "PENTAX K-1 Mark II" reads
		// "RICOH PENTAX K-1 Mark II".
		name = firstWord(mk) + " " + model
	}
	if leicaM.MatchString(name) {
		name += " rangefinder (manual focus, often fast lenses shot wide open)"
	}
	if strings.ContainsRune("AEIOUaeiou", rune(name[0])) {
		return "an " + name
	}
	return "a " + name
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return strings.TrimRight(f[0], ",.")
	}
	return ""
}
