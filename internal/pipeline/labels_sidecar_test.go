package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/labels"
)

// judge --write-xmp with labels: every labelled frame's sidecar carries your verdict
// and stars, not only frames in a set or whose decision changed after grouping. A
// lone frame the model keeps but you culled used to keep a green, model-verdict
// sidecar while --sort moved it into cull/.
func TestJudgeSidecarsFollowLabels(t *testing.T) {
	dir, b := shoot(t) // L1 missed_focus, L2 sharp, L3 soft; no sets (Seq off)
	c := cfg(dir)
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	c.Labels = map[string]labels.Entry{"L1000002.DNG": {File: "L1000002.DNG", Label: "cull", Stars: 2}}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	x, err := os.ReadFile(filepath.Join(dir, "L1000002.xmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`xmp:Label="Red"`, `xmp:Rating="2"`, "cull:cull", "cull:labeled"} {
		if !strings.Contains(string(x), want) {
			t.Errorf("sidecar lacks %s:\n%s", want, x)
		}
	}
	// An unlabelled frame keeps the model's verdict.
	if x, _ := os.ReadFile(filepath.Join(dir, "L1000003.xmp")); !strings.Contains(string(x), `xmp:Label="Yellow"`) {
		t.Errorf("L3 sidecar:\n%s", x)
	}
}
