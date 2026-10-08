// Package develop turns a decided shoot into client JPEGs through LightCraft: cull
// stays the decision layer (keep set, exposure, crop, in the sidecars), LightCraft the
// render layer. Nothing here decodes or renders a raw: each chunk of frames is one
// headless lightcraft-cli run, driven by a script of its commands.
package develop

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Recipe is a shoot's develop settings, the same for every frame; what differs per
// frame (the preset, cull's EV) comes from the frame's camera and sidecar.
type Recipe struct {
	WB         string `json:"wb"`         // develop.wb mode for every frame; "" keeps the file's (as shot)
	Straighten bool   `json:"straighten"` // crop.autoStraighten
	// Presets applies the camera model's preset (PresetFor). A body without one, or
	// every body when off, gets develop.auto instead.
	Presets bool   `json:"presets"`
	Export  Export `json:"export"`
}

// Export is app.export's settings.
type Export struct {
	Format   string `json:"format"`    // jpeg
	LongEdge int    `json:"long_edge"` // pixels; 0 = full size
	Quality  int    `json:"quality"`   // 1-100
}

// DefaultRecipe is client delivery: auto white balance, levelled horizons, the
// camera's preset, 3000 px JPEGs at quality 95.
func DefaultRecipe() Recipe {
	return Recipe{WB: "auto", Straighten: true, Presets: true, Export: Export{Format: "jpeg", LongEdge: 3000, Quality: 95}}
}

// wbModes are develop.wb's modes that need no temperature ("custom" does).
var wbModes = []string{"asShot", "auto", "daylight", "cloudy", "shade", "tungsten", "fluorescent", "flash"}

// Validate checks the recipe before anything runs.
func (r Recipe) Validate() error {
	if r.WB != "" && !slices.Contains(wbModes, r.WB) {
		return fmt.Errorf("white balance %q: want one of %s, or none", r.WB, strings.Join(wbModes, ", "))
	}
	if r.Export.Format != "jpeg" {
		return fmt.Errorf("export format %q: only jpeg is supported", r.Export.Format)
	}
	if r.Export.LongEdge < 0 {
		return fmt.Errorf("long edge %d: want pixels, or 0 for full size", r.Export.LongEdge)
	}
	if r.Export.Quality < 1 || r.Export.Quality > 100 {
		return fmt.Errorf("quality %d: want 1-100", r.Export.Quality)
	}
	return nil
}

// Frame is one keep to develop.
type Frame struct {
	Name    string    // base name (L1001525.DNG): the state's key, and the JPEG's stem
	Path    string    // where it is now (the shoot folder or keep/)
	Model   string    // EXIF Model: picks the preset
	Size    int64     // the DNG's
	ModTime time.Time // the DNG's
	// From the frame's sidecar (cull's, or one a user edited), which LightCraft also
	// reads on import: EV is re-asserted after the auto stages, which overwrite it.
	Sidecar    string   // path; "" = none
	SidecarSum string   // SHA-256 of the sidecar's bytes; "" = none
	EV         *float64 // crs:Exposure2012
	Crop       bool     // crs:HasCrop (LightCraft applies it on import; no stage touches it)
}

// Command is one LightCraft command: a line of a `lightcraft-cli run --script` file.
type Command struct {
	Command string         `json:"command"`
	Params  map[string]any `json:"params,omitempty"`
}

// Steps is the frame's commands, from making it the active photo (develop.auto,
// develop.wb and crop.autoStraighten act on the active photo only) to its export to
// out. id is its LightCraft photo id.
//
// Every LightCraft stage here sets values outright, so their order decides who wins:
//   - the preset sets every control develop.auto sets, so auto runs only where no
//     preset applies (it would be overwritten);
//   - auto, and the preset, overwrite the exposure LightCraft read from cull's sidecar,
//     so cull's EV is set again last. It is relative to the camera's rendering, which
//     the preset reproduces: preset exposure + EV. Without a preset the base is
//     LightCraft's default rendering (0), and the EV replaces auto's exposure;
//   - nothing sets the crop rectangle, so cull's sidecar crop stands (straighten
//     only levels it).
func (r Recipe) Steps(f Frame, id int, out string) []Command {
	ids := []int{id}
	cmds := []Command{{"library.select", map[string]any{"ids": ids, "active": id}}}
	p, preset := r.preset(f)
	if r.WB != "" {
		cmds = append(cmds, Command{"develop.wb", map[string]any{"mode": r.WB}})
	}
	if !preset {
		cmds = append(cmds, Command{"develop.auto", nil})
	}
	if r.Straighten {
		cmds = append(cmds, Command{"crop.autoStraighten", nil})
	}
	if preset {
		cmds = append(cmds, Command{"preset.apply", map[string]any{"id": p.ID, "ids": ids}})
	}
	if f.EV != nil {
		v := math.Round((p.Exposure+*f.EV)*100) / 100
		if v == 0 {
			v = 0 // not -0
		}
		cmds = append(cmds, Command{"develop.set", map[string]any{"control": "light.exposure", "value": v, "ids": ids}})
	}
	return append(cmds, Command{"app.export", map[string]any{"ids": ids, "path": out, "format": r.Export.Format,
		"longEdge": r.Export.LongEdge, "quality": r.Export.Quality}})
}

// preset is the frame's preset, when the recipe applies presets and its camera has one.
func (r Recipe) preset(f Frame) (Preset, bool) {
	if !r.Presets {
		return Preset{}, false
	}
	return PresetFor(f.Model)
}

// Fingerprint identifies everything a frame's JPEG is made from: its commands, the
// preset's bytes, the sidecar, and the DNG's size and mtime. A frame whose fingerprint
// matches its recorded export is up to date; any change develops it again. Its path is
// left out, so decide --sort moving it into keep/ redoes nothing.
func (r Recipe) Fingerprint(f Frame) string {
	p, _ := r.preset(f)
	b, _ := json.Marshal(struct {
		Steps   []Command
		Preset  []byte
		Sidecar string
		Size    int64
		ModTime int64
	}{r.Steps(f, 0, ""), p.Data, f.SidecarSum, f.Size, f.ModTime.UnixNano()})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Preset is a LightCraft preset cull ships, keyed by camera model.
type Preset struct {
	ID       string  // LightCraft's preset id, for preset.apply
	File     string  // its file name: preset.import reads a copy in the work folder
	Exposure float64 // its light.exposure: the base cull's EV adds to
	Data     []byte
}

//go:embed presets/*.lcpreset
var presetFiles embed.FS

// presetsByModel maps an EXIF Model (upper case) to its preset's file. Starts with the
// M10-R, the body docs/lightcraft/README.md validated it on.
var presetsByModel = map[string]string{
	"LEICA M10-R": "leica-m10r-std.lcpreset",
}

// PresetFor is the preset for a camera model (EXIF Model; case and padding ignored).
//
// It panics, rather than returning an error, on a table entry whose embedded file is
// missing or malformed. Both the table and the files are compiled into the binary (a
// literal map, go:embed), so no input or runtime state can reach that path: only a
// mistake made when adding a preset can. TestEveryPresetLoads loads every entry, so make
// test fails on that mistake before a binary carrying it is built. An error return would
// thread through Steps, Fingerprint and Prepare to handle a case no user can cause.
func PresetFor(model string) (Preset, bool) {
	file, ok := presetsByModel[strings.ToUpper(strings.TrimSpace(model))]
	if !ok {
		return Preset{}, false
	}
	b, err := presetFiles.ReadFile("presets/" + file)
	if err != nil {
		panic(err) // the table names a file the binary doesn't embed: a build error
	}
	var doc struct {
		Presets []struct {
			ID       string `json:"id"`
			Settings struct {
				Light struct {
					Exposure float64 `json:"exposure"`
				} `json:"light"`
			} `json:"settings"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Presets) != 1 || doc.Presets[0].ID == "" {
		panic(fmt.Sprintf("embedded preset %s: want one preset with an id (%v)", file, err))
	}
	return Preset{ID: doc.Presets[0].ID, File: file, Exposure: doc.Presets[0].Settings.Light.Exposure, Data: b}, true
}
