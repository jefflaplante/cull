package report

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCSVIncludesFocusTargetColumns(t *testing.T) {
	r := &Report{SchemaVersion: SchemaVersion, Results: []Result{{
		File:        "/x/L1.DNG",
		FocusTarget: &FocusTarget{Source: "face", FaceQ: 102.5, Faces: 1, SubjectSharpness: 0.183, LandedSharpness: 0.2},
	}}}
	p := filepath.Join(t.TempDir(), "r.csv")
	if err := r.WriteCSV(p); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	header := strings.Join(rows[0], ",")
	if !strings.Contains(header, "focus_source,face_q,subject_sharpness,landed_sharpness") {
		t.Fatalf("header %s", header)
	}
	row := strings.Join(rows[1], ",")
	if !strings.Contains(row, "face,102.5,0.183,0.200") {
		t.Fatalf("row %s", row)
	}
}
