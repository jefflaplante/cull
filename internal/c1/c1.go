// Package c1 generates AppleScript that applies a report to the open Capture One
// document: rating, color tag and keyword per decision, and optionally the
// suggested exposure and crop. Capture One doesn't reliably read Adobe develop
// settings from sidecars, so edits go through its scripting interface.
//
// Verified against Capture One 16.7.2's CaptureOne.sdef: variant has rw
// "rating", "color tag" (integer), "adjustments" (with "exposure"), "crop"
// ({centerX, centerY, width, height}); image has "name", "path", "dimensions";
// "apply keyword <existing keyword> to {variants}". NOT in the dictionary, and
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

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
)

// Options select what the script writes.
type Options struct {
	Rating, Label, Keyword bool
	Exposure               bool // suggested EV for frames the model marked fixable
	Crop                   bool // suggested crop for frames marked croppable
}

var rating = map[eval.Decision]int{eval.Keep: 3, eval.Review: 2, eval.Cull: 1}

// colorTag follows Capture One's usual numbering (0 none, 1 red, 2 orange,
// 3 yellow, 4 green, 5 blue, 6 pink, 7 purple): not in the dictionary; confirm
// with Probe. Keep leaves the tag alone, like the XMP sidecars.
var colorTag = map[eval.Decision]int{eval.Review: 3, eval.Cull: 1}

// Script returns the AppleScript for every decided frame in the report.
func Script(rep *report.Report, o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- gophotocull apply-c1, generated %s. Review before running.\n", time.Now().Format(time.RFC3339))
	b.WriteString(helpers)
	b.WriteString("tell application \"Capture One\"\n\tset doc to current document\n\tset notFound to {}\n")
	for _, r := range rep.Results {
		if r.Error != "" || r.Decision == "" || r.Evaluation == nil {
			continue
		}
		name := filepath.Base(r.File)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		fmt.Fprintf(&b, "\n\t-- %s: %s\n", name, r.Decision)
		fmt.Fprintf(&b, "\tset imgs to my matchImages(doc, %s, %s)\n", quote(name), quote(stem))
		fmt.Fprintf(&b, "\tif (count of imgs) is 0 then set end of notFound to %s\n", quote(name))
		b.WriteString("\trepeat with img in imgs\n")
		crop := o.Crop && r.Evaluation.Composition.Status == "croppable" && r.Evaluation.Composition.Crop.Apply
		if crop {
			b.WriteString("\t\tset d to dimensions of img\n\t\tset w to item 1 of d\n\t\tset h to item 2 of d\n")
		}
		b.WriteString("\t\trepeat with v in (variants of img)\n")
		if o.Rating {
			fmt.Fprintf(&b, "\t\t\tset rating of v to %d\n", rating[r.Decision])
		}
		if tag, ok := colorTag[r.Decision]; ok && o.Label {
			fmt.Fprintf(&b, "\t\t\tset color tag of v to %d\n", tag)
		}
		if o.Keyword {
			fmt.Fprintf(&b, "\t\t\tset k to my ensureKeyword(doc, %s)\n", quote("gophotocull:"+string(r.Decision)))
			b.WriteString("\t\t\tif k is not missing value then apply keyword k to {v}\n")
		}
		if o.Exposure && r.Evaluation.Exposure.Status == "fixable" {
			fmt.Fprintf(&b, "\t\t\tset exposure of adjustments of v to %s\n", num(r.Evaluation.Exposure.EVAdjust))
		}
		if crop {
			c := r.Evaluation.Composition.Crop
			fmt.Fprintf(&b, "\t\t\tset crop of v to {%s * w, %s * h, %s * w, %s * h}\n",
				num((c.Left+c.Right)/2), num((c.Top+c.Bottom)/2), num(c.Right-c.Left), num(c.Bottom-c.Top))
		}
		b.WriteString("\t\tend repeat\n\tend repeat\n")
	}
	b.WriteString("\n\tif (count of notFound) is 0 then return \"done\"\n")
	b.WriteString("\treturn \"done; not found in this document: \" & my joinList(notFound)\nend tell\n")
	return b.String()
}

const helpers = `
on matchImages(doc, fileName, stem)
	tell application "Capture One"
		set found to (every image of doc whose name is fileName)
		if (count of found) is 0 then set found to (every image of doc whose name is stem)
		return found
	end tell
end matchImages

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
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
