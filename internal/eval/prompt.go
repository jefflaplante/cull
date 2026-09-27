package eval

import "fmt"

// SystemPrompt encodes the evaluation priorities. The final keep/review/cull decision
// is made by Policy in Go; the model only assesses.
func SystemPrompt(minCropArea float64) string {
	return fmt.Sprintf(`You are an experienced photo editor culling raw files from a Leica M11-P rangefinder (manual focus, often fast lenses shot wide open). For each image you receive the full frame downscaled from the camera's embedded JPEG preview, the highest-detail regions of that preview at native resolution, and measured statistics. Assess in strict priority order.

1. SHARPNESS (primary). Decide whether the plane of focus landed on the intended subject (for people: the nearer eye). Thin depth of field and background blur are intentional and not faults. Judge acuity from the native-resolution regions; the downscaled frame hides soft focus. If the sharpest region is not the subject (e.g. focus on an ear, hair, or background), that is missed_focus. Distinguish missed focus from motion_blur (directional smear from subject motion or camera shake). The preview is in-camera sharpened and JPEG-compressed: judge relative acuity, not absolute micro-contrast. The Laplacian figures are relative focus measures, useful only as supporting evidence.
   status: sharp | acceptable | soft | missed_focus | motion_blur

2. EXPOSURE (fix, don't cull). Estimate the EV adjustment that renders the subject well. The preview is tone-mapped; the raw file usually retains extra highlight headroom and deep-shadow data beyond what the preview shows. Use "clipped" only when large, important areas (skin, the subject, sky that should hold texture) are featureless white or black. Small speculars and light sources clipping is normal. Within ±0.3 EV is "good".
   status: good | fixable | clipped

3. COMPOSITION (fix by cropping where possible). List concrete issues: tilted horizon or verticals, awkward subject cuts, distracting edge elements, excess dead space. If a crop fixes it without cutting the subject, set crop.apply=true with normalized edges (left, top, right, bottom in 0-1, measured from the top-left of the image as displayed), preferring standard aspect ratios (3:2, 4:5, 1:1, 16:9) and retaining at least %.0f%% of the frame area. Report tilt as straighten_degrees (positive = rotate clockwise). If cropping cannot fix it, status "flawed".
   status: good | croppable | flawed

Scores are 0-10: 5 = usable, 7 = good, 9+ = exceptional. Do not inflate. Keep text fields terse.`, 100*minCropArea)
}

var toolSchema = map[string]any{
	"type":     "object",
	"required": []string{"sharpness", "exposure", "composition", "notes"},
	"properties": map[string]any{
		"sharpness": map[string]any{
			"type":     "object",
			"required": []string{"score", "status", "focus_target"},
			"properties": map[string]any{
				"score":        map[string]any{"type": "number", "minimum": 0, "maximum": 10},
				"status":       map[string]any{"type": "string", "enum": []string{"sharp", "acceptable", "soft", "missed_focus", "motion_blur"}},
				"focus_target": map[string]any{"type": "string", "description": "Where the plane of focus landed vs. where it should be."},
			},
		},
		"exposure": map[string]any{
			"type":     "object",
			"required": []string{"score", "status", "ev_adjust", "clipping", "reason"},
			"properties": map[string]any{
				"score":     map[string]any{"type": "number", "minimum": 0, "maximum": 10},
				"status":    map[string]any{"type": "string", "enum": []string{"good", "fixable", "clipped"}},
				"ev_adjust": map[string]any{"type": "number", "minimum": -4, "maximum": 4},
				"clipping":  map[string]any{"type": "string", "enum": []string{"none", "highlights", "shadows", "both"}},
				"reason":    map[string]any{"type": "string"},
			},
		},
		"composition": map[string]any{
			"type":     "object",
			"required": []string{"score", "status", "issues", "crop", "straighten_degrees"},
			"properties": map[string]any{
				"score":  map[string]any{"type": "number", "minimum": 0, "maximum": 10},
				"status": map[string]any{"type": "string", "enum": []string{"good", "croppable", "flawed"}},
				"issues": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"crop": map[string]any{
					"type":     "object",
					"required": []string{"apply", "left", "top", "right", "bottom"},
					"properties": map[string]any{
						"apply":  map[string]any{"type": "boolean"},
						"left":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						"top":    map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						"right":  map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						"bottom": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					},
				},
				"straighten_degrees": map[string]any{"type": "number", "minimum": -45, "maximum": 45},
			},
		},
		"notes": map[string]any{"type": "string"},
	},
}
