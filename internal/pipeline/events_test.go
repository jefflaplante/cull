package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jefflaplante/cull/internal/ui"
)

// recordSink keeps every event.
type recordSink struct {
	mu sync.Mutex
	ev []ui.Event
}

func (r *recordSink) Emit(e ui.Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }
func (r *recordSink) Close() error    { return nil }

func (r *recordSink) notes(l ui.Level) []string {
	var out []string
	for _, e := range r.ev {
		if e.Note != nil && e.Note.Level == l {
			out = append(out, e.Note.Text)
		}
	}
	return out
}

func TestRunEmitsStagesAndFrames(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	rs := &recordSink{}
	c.UI = rs
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	var start, adds, done, frames int
	for _, e := range rs.ev {
		switch {
		case e.Stage != nil && e.Stage.Done:
			done++
		case e.Stage != nil && e.Stage.Total == 4 && e.Stage.Add == 0:
			start++
			if e.Stage.Name != "judge" || e.Stage.Unit != "frames" {
				t.Errorf("stage %+v", *e.Stage)
			}
		case e.Stage != nil && e.Stage.Add > 0:
			adds++
		case e.Frame != nil:
			frames++
			if e.Frame.Decision == "" || !strings.HasPrefix(e.Frame.Line, "[") {
				t.Errorf("frame %+v", *e.Frame)
			}
		}
	}
	if start != 1 || adds != 4 || done != 1 || frames != 4 {
		t.Fatalf("start %d adds %d done %d frames %d", start, adds, done, frames)
	}
}

func TestScanStageIsNamedScan(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.DryRun = true
	rs := &recordSink{}
	c.UI = rs
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range rs.ev {
		if e.Stage != nil && e.Stage.Total > 0 {
			if e.Stage.Name != "scan" {
				t.Fatalf("dry run stage named %q", e.Stage.Name)
			}
			return
		}
	}
	t.Fatal("no stage start")
}

func TestVerboseNotesNameTheFocusTarget(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	rs := &recordSink{}
	c.UI = rs
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	v := rs.notes(ui.Verbose)
	if len(v) < 4 {
		t.Fatalf("want a verbose note per frame, got %q", v)
	}
	for _, s := range v[:4] {
		if !strings.Contains(s, "focus:") || !strings.Contains(s, "tokens in=") {
			t.Fatalf("verbose note %q lacks focus target or tokens", s)
		}
	}
}

func TestNilUIKeepsTodaysLines(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	var b strings.Builder
	c.Log = &b
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "[4/4] ") || strings.Contains(b.String(), "focus:") {
		t.Fatalf("plain log changed:\n%s", b.String())
	}
}
