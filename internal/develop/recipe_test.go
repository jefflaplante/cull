package develop

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func ev(v float64) *float64 { return &v }

func TestPresetFor(t *testing.T) {
	cases := []struct {
		model string
		id    string // "" = none
	}{
		{"LEICA M10-R", "leica-m10r-std"},
		{" leica m10-r ", "leica-m10r-std"}, // EXIF padding and case don't matter
		{"LEICA M11-P", ""},
		{"LEICA M10", ""}, // a different body, not a prefix match
		{"", ""},
	}
	for _, c := range cases {
		p, ok := PresetFor(c.model)
		if ok != (c.id != "") || p.ID != c.id {
			t.Errorf("PresetFor(%q) = %q, %v; want %q", c.model, p.ID, ok, c.id)
		}
	}
}

// Every table entry loads: PresetFor panics on a broken one (see its comment), so this
// is what keeps that panic out of every tested build.
func TestEveryPresetLoads(t *testing.T) {
	if len(presetsByModel) == 0 {
		t.Fatal("no presets")
	}
	for model, file := range presetsByModel {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s (%s): %v", model, file, r)
				}
			}()
			p, ok := PresetFor(model)
			if !ok || p.ID == "" || p.File != file || len(p.Data) == 0 {
				t.Errorf("%s: %+v, %v", model, p, ok)
			}
		}()
	}
}

// The embedded preset is the documented one: the copy cull ships and the one
// docs/lightcraft/README.md validates can't drift apart.
func TestEmbeddedPresetMatchesDocs(t *testing.T) {
	doc, err := os.ReadFile("../../docs/lightcraft/leica-m10r-std.lcpreset")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := PresetFor("LEICA M10-R")
	if !bytes.Equal(p.Data, doc) {
		t.Error("internal/develop/presets/leica-m10r-std.lcpreset differs from docs/lightcraft/leica-m10r-std.lcpreset")
	}
	if p.Exposure != 0.3 || p.File != "leica-m10r-std.lcpreset" {
		t.Errorf("preset exposure %v file %q", p.Exposure, p.File)
	}
}

func TestSteps(t *testing.T) {
	const out = "/x/.cull-develop-1.A.jpg"
	sel := Command{"library.select", map[string]any{"ids": []int{7}, "active": 7}}
	exp := Command{"app.export", map[string]any{"ids": []int{7}, "path": out, "format": "jpeg", "longEdge": 3000, "quality": 95}}
	wb := Command{"develop.wb", map[string]any{"mode": "auto"}}
	straighten := Command{"crop.autoStraighten", nil}
	auto := Command{"develop.auto", nil}
	preset := Command{"preset.apply", map[string]any{"id": "leica-m10r-std", "ids": []int{7}}}
	exposure := func(v float64) Command {
		return Command{"develop.set", map[string]any{"control": "light.exposure", "value": v, "ids": []int{7}}}
	}
	def := DefaultRecipe()
	noPresets := def
	noPresets.Presets = false
	bare := def
	bare.WB, bare.Straighten = "", false
	cases := []struct {
		name string
		r    Recipe
		f    Frame
		want []Command
	}{
		// A preset sets every control develop.auto sets, so auto is left out where one applies.
		{"M10-R, no EV", def, Frame{Model: "LEICA M10-R"}, []Command{sel, wb, straighten, preset, exp}},
		// cull's EV is relative to the camera's rendering, which the preset reproduces:
		// it adds to the preset's own +0.3.
		{"M10-R, EV +0.5", def, Frame{Model: "LEICA M10-R", EV: ev(0.5)}, []Command{sel, wb, straighten, preset, exposure(0.8), exp}},
		{"M10-R, EV -0.3 rounds to 0", def, Frame{Model: "LEICA M10-R", EV: ev(-0.3)}, []Command{sel, wb, straighten, preset, exposure(0), exp}},
		{"unknown body: auto, no preset", def, Frame{Model: "LEICA M11-P"}, []Command{sel, wb, auto, straighten, exp}},
		// Without a preset the base is LightCraft's default rendering: cull's EV replaces auto's exposure.
		{"unknown body, EV", def, Frame{Model: "LEICA M11-P", EV: ev(-0.7)}, []Command{sel, wb, auto, straighten, exposure(-0.7), exp}},
		{"presets off", noPresets, Frame{Model: "LEICA M10-R", EV: ev(0.5)}, []Command{sel, wb, auto, straighten, exposure(0.5), exp}},
		{"no wb, no straighten", bare, Frame{Model: "LEICA M10-R"}, []Command{sel, preset, exp}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.r.Steps(c.f, 7, out)
			if !reflect.DeepEqual(got, c.want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(c.want)
				t.Errorf("got  %s\nwant %s", g, w)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	mt := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	base := Frame{Name: "A.DNG", Path: "/s/A.DNG", Model: "LEICA M10-R", Size: 100, ModTime: mt, SidecarSum: "s1"}
	r := DefaultRecipe()
	fp := r.Fingerprint(base)
	if fp != r.Fingerprint(base) {
		t.Fatal("not deterministic")
	}
	moved := base
	moved.Path = "/s/keep/A.DNG" // decide --sort moved it: the same frame, nothing to redo
	if r.Fingerprint(moved) != fp {
		t.Error("a move into keep/ changed the fingerprint")
	}
	change := map[string]func(*Recipe, *Frame){
		"sidecar":  func(_ *Recipe, f *Frame) { f.SidecarSum = "s2" },
		"ev":       func(_ *Recipe, f *Frame) { f.EV = ev(0.5) },
		"size":     func(_ *Recipe, f *Frame) { f.Size = 101 },
		"mtime":    func(_ *Recipe, f *Frame) { f.ModTime = mt.Add(time.Second) },
		"model":    func(_ *Recipe, f *Frame) { f.Model = "LEICA M11-P" },
		"wb":       func(r *Recipe, _ *Frame) { r.WB = "asShot" },
		"quality":  func(r *Recipe, _ *Frame) { r.Export.Quality = 90 },
		"longEdge": func(r *Recipe, _ *Frame) { r.Export.LongEdge = 2048 },
	}
	for name, fn := range change {
		r2, f2 := DefaultRecipe(), base
		fn(&r2, &f2)
		if r2.Fingerprint(f2) == fp {
			t.Errorf("%s change kept the fingerprint", name)
		}
	}
}

func TestRecipeValidate(t *testing.T) {
	bad := map[string]func(*Recipe){
		"format":   func(r *Recipe) { r.Export.Format = "png" },
		"quality":  func(r *Recipe) { r.Export.Quality = 101 },
		"longEdge": func(r *Recipe) { r.Export.LongEdge = -1 },
		"wb":       func(r *Recipe) { r.WB = "warm" },
	}
	if err := DefaultRecipe().Validate(); err != nil {
		t.Fatalf("default: %v", err)
	}
	for name, fn := range bad {
		r := DefaultRecipe()
		fn(&r)
		if r.Validate() == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
