// Package c1 generates AppleScript that applies a report to the open Capture One
// document: rating from your stars (--labels), color tag and keyword per verdict
// (yours where you labeled, else the model's), and optionally the
// suggested exposure and crop. Capture One doesn't reliably read Adobe develop
// settings from sidecars, so edits go through its scripting interface.
//
// Verified against Capture One 16.7.2's CaptureOne.sdef: variant has rw
// "rating", "color tag" (integer), "adjustments" (with "exposure"), "crop"
// ({centerX, centerY, width, height}); image has "name", "path", "dimensions";
// "apply keyword <existing keyword> to {variants}". Images are matched by path
// (where the frame lives now), and by name only when exactly one image has it,
// since file numbers repeat across shoots. NOT in the dictionary, and
// so hedged or checked with Probe first: the color-tag numbering, whether image
// names include the extension, and the orientation "dimensions"/"crop" use.
package c1

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
)

// Options select what the script writes. Exposure applies your EV from review
// (o.Labels) as is, and otherwise the model's suggestion only where the image still
// has Capture One's default exposure (0); Crop applies only to an uncropped frame.
type Options struct {
	Rating, Label, Keyword bool
	Exposure               bool                    // suggested EV for frames the model marked fixable
	Crop                   bool                    // suggested crop for frames marked croppable
	Labels                 map[string]labels.Entry // your verdicts and stars by base name; nil = the model's verdicts, no ratings
}

// colorTag follows Capture One's usual numbering (0 none, 1 red, 2 orange,
// 3 yellow, 4 green, 5 blue, 6 pink, 7 purple): not in the dictionary; confirm
// with Probe. Same colours as the XMP sidecars.
var colorTag = map[eval.Decision]int{eval.Keep: 4, eval.Review: 3, eval.Cull: 1}

// Script returns the AppleScript for every frame with a verdict (the user's label
// in o.Labels, else the model's decision) or with the user's stars.
func Script(rep *report.Report, o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- cull apply-c1, generated %s. Review before running.\n", time.Now().Format(time.RFC3339))
	b.WriteString(helpers)
	b.WriteString("tell application \"Capture One\"\n\tset doc to current document\n\tset notFound to {}\n")
	for _, r := range rep.Results {
		l := o.Labels[filepath.Base(r.File)]
		d, yours := labels.Effective(r, l)
		if r.Error != "" || (d == "" && l.Stars == 0) {
			continue
		}
		name := filepath.Base(r.File)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		who := "model"
		if yours {
			who = "yours"
		}
		where := r.File // where the frame lives now: Capture One imported it from there
		if r.MovedTo != "" {
			where = r.MovedTo
		}
		fmt.Fprintf(&b, "\n\t-- %s: %s (%s)\n", comment(name), d, who)
		fmt.Fprintf(&b, "\tset imgs to my matchImages(doc, %s, %s, %s)\n", quote(where), quote(name), quote(stem))
		fmt.Fprintf(&b, "\tif (count of imgs) is 0 then set end of notFound to %s\n", quote(name))
		b.WriteString("\trepeat with img in imgs\n")
		e := r.Evaluation
		crop := o.Crop && e != nil && e.Composition.Status == "croppable" && e.Composition.Crop.Apply
		if crop {
			b.WriteString("\t\tset d to dimensions of img\n\t\tset w to item 1 of d\n\t\tset h to item 2 of d\n")
		}
		b.WriteString("\t\trepeat with v in (variants of img)\n")
		if o.Rating && l.Stars > 0 { // only your stars: never reset ratings made in Capture One
			fmt.Fprintf(&b, "\t\t\tset rating of v to %d\n", l.Stars)
		}
		if tag, ok := colorTag[d]; ok && o.Label {
			fmt.Fprintf(&b, "\t\t\tset color tag of v to %d\n", tag)
		}
		if o.Keyword && d != "" {
			kws := []string{"cull:" + string(d)}
			if yours {
				kws = append(kws, "cull:labeled")
			}
			if r.Group != nil && r.Group.Best {
				kws = append(kws, "cull:best")
			}
			for _, kw := range kws {
				fmt.Fprintf(&b, "\t\t\tset k to my ensureKeyword(doc, %s)\n", quote(kw))
				b.WriteString("\t\t\tif k is not missing value then apply keyword k to {v}\n")
			}
		}
		switch {
		case o.Exposure && l.EV != nil:
			// Yours, set in review: it wins over whatever Capture One holds, since it
			// is the last thing you chose.
			fmt.Fprintf(&b, "\t\t\tset exposure of adjustments of v to %s\n", num(*l.EV))
		case o.Exposure && e != nil && e.Exposure.Status == "fixable":
			// The model's suggestion: only over the default, so an exposure set in
			// Capture One is never overwritten.
			fmt.Fprintf(&b, "\t\t\tif (exposure of adjustments of v) is 0 then set exposure of adjustments of v to %s\n", num(e.Exposure.EVAdjust))
		}
		if crop {
			c := e.Composition.Crop
			fmt.Fprintf(&b, "\t\t\tif my isFullFrame(crop of v, w, h) then set crop of v to {%s * w, %s * h, %s * w, %s * h}\n",
				num((c.Left+c.Right)/2), num((c.Top+c.Bottom)/2), num(c.Right-c.Left), num(c.Bottom-c.Top))
		}
		b.WriteString("\t\tend repeat\n\tend repeat\n")
	}
	b.WriteString("\n\tif (count of my ambiguous) > 0 then return \"not applied: several images share these names and none is at the report's path (ambiguous): \" & my joinList(my ambiguous) & \"; not found: \" & my joinList(notFound)\n")
	b.WriteString("\n\tif (count of notFound) is 0 then return \"done\"\n")
	b.WriteString("\treturn \"done; not found in this document: \" & my joinList(notFound)\nend tell\n")
	return b.String()
}

const helpers = `
property ambiguous : {}

on matchImages(doc, posixPath, fileName, stem)
	tell application "Capture One"
		try
			set found to (every image of doc whose path is posixPath)
			if (count of found) > 0 then return found
		end try
		-- By name only when exactly one image has it: file numbers repeat across
		-- shoots and bodies, and a verdict must never land on another shoot's frame.
		set found to (every image of doc whose name is fileName)
		if (count of found) is 0 then set found to (every image of doc whose name is stem)
		if (count of found) is 1 then return found
		if (count of found) > 1 then set end of my ambiguous to fileName
		return {}
	end tell
end matchImages

-- An untouched crop covers the whole image (within a pixel). Any other crop was
-- set in Capture One and is kept. If the crop's orientation doesn't match
-- dimensions (unverified), this is false and the suggestion is skipped: the safe
-- way to be wrong.
on isFullFrame(cr, w, h)
	return ((item 3 of cr) ≥ w - 1) and ((item 4 of cr) ≥ h - 1)
end isFullFrame

on ensureKeyword(doc, kname)
	tell application "Capture One"
		try
			return first keyword of doc whose name is kname
		end try
		try
			return make new keyword at doc with properties {name:kname}
		end try
		return missing value
	end tell
end ensureKeyword

` + joinListHelper

const joinListHelper = `on joinList(l)
	set AppleScript's text item delimiters to ", "
	set s to l as text
	set AppleScript's text item delimiters to ""
	return s
end joinList

`

// Probe is a read-only script listing the first n images of the open document
// with the values the write script depends on, to check against the Capture One
// UI before running it.
func Probe(n int) string {
	return joinListHelper + fmt.Sprintf(`tell application "Capture One"
	set doc to current document
	set out to "document: " & (name of doc) & linefeed
	set imgs to images of doc
	set n to %d
	if (count of imgs) < n then set n to (count of imgs)
	repeat with i from 1 to n
		set img to item i of imgs
		set out to out & "image name: " & (name of img) & " | path: " & (path of img) & " | dimensions: " & my joinList(dimensions of img) & linefeed
		repeat with v in (variants of img)
			set out to out & "  variant " & (name of v) & ": rating " & (rating of v) & ", color tag " & (color tag of v) & ", crop " & my joinList(crop of v) & ", exposure " & (exposure of (adjustments of v)) & linefeed
		end repeat
	end repeat
	return out
end tell
`, n)
}

// Run pipes a script to osascript and returns its output.
func Run(osascript, script string) (string, error) {
	cmd := exec.Command(osascript, "-")
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// quote makes an AppleScript string literal.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

// comment makes s safe inside an AppleScript "--" comment: any line break would
// end the comment and run the rest as code, and ¬ would continue it onto the
// next line. File names can contain both.
func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '¬' || r == '\u2028' || r == '\u2029' {
			return '?'
		}
		return r
	}, s)
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
