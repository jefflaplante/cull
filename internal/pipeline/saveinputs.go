package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
)

// saveInputs writes req's images and text in send order, so calibration can see
// exactly what the model judged. names label the JPEG parts in order
// (full, subject, landed-1, ...).
func saveInputs(dir, base string, req llm.Request, names []string, ft *report.FocusTarget) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	type part struct {
		Text  string `json:"text,omitempty"`
		Image string `json:"image,omitempty"`
	}
	rec := struct {
		SchemaName  string              `json:"schema_name"`
		System      string              `json:"system"`
		Parts       []part              `json:"parts"`
		FocusTarget *report.FocusTarget `json:"focus_target,omitempty"`
	}{SchemaName: req.SchemaName, System: req.System, FocusTarget: ft}
	n := 0
	for _, p := range req.Parts {
		if p.JPEG == nil {
			rec.Parts = append(rec.Parts, part{Text: p.Text})
			continue
		}
		name := fmt.Sprintf("image-%d", n+1)
		if n < len(names) {
			name = names[n]
		}
		n++
		file := base + "." + name + ".jpg"
		if err := os.WriteFile(filepath.Join(dir, file), p.JPEG, 0o644); err != nil {
			return err
		}
		rec.Parts = append(rec.Parts, part{Image: file})
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, base+".inputs.json"), b, 0o644)
}

// inputsBase names a frame's saved inputs by its path relative to the shoot, so
// recursive runs with repeated file names in different folders don't collide.
func inputsBase(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	return strings.ReplaceAll(rel, string(filepath.Separator), "__")
}
