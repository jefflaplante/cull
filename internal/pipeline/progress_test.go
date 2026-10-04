package pipeline

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/ui"
)

// recorder keeps every stage event: start total, units added, whether it finished.
type recorder struct {
	mu     sync.Mutex
	total  map[string]int64
	added  map[string]int64
	done   map[string]bool
	starts map[string]int
}

func newRecorder() *recorder {
	return &recorder{total: map[string]int64{}, added: map[string]int64{}, done: map[string]bool{}, starts: map[string]int{}}
}

func (r *recorder) Emit(e ui.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := e.Stage; s != nil {
		if s.Start {
			r.starts[s.Name]++
			r.total[s.Name] = s.Total
		}
		r.added[s.Name] += s.Add
		if s.Done {
			r.done[s.Name] = true
		}
	}
}
func (r *recorder) Close() error { return nil }

func (r *recorder) finished(t *testing.T, name string, total int64) {
	t.Helper()
	if r.starts[name] == 0 || r.total[name] != total || r.added[name] != total || !r.done[name] {
		t.Errorf("stage %s: starts %d total %d added %d done %v; want total and added %d, done",
			name, r.starts[name], r.total[name], r.added[name], r.done[name], total)
	}
}

// judge --write-xmp --sort shows the sidecars it writes and the frames it sorts, not
// a silent pause after the judge bar fills.
func TestJudgeShowsSidecarAndSortProgress(t *testing.T) {
	dir, b := shoot(t) // 3 frames
	c := moveCfg(dir)
	c.MoveCulled, c.Sort, c.WriteXMP = false, true, true
	c.Labels = map[string]labels.Entry{"L1000002.DNG": {File: "L1000002.DNG", Label: "cull"}} // labelled: rewritten at the end
	rec := newRecorder()
	c.UI = rec
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	rec.finished(t, "sort", 3)
	if rec.starts["sidecars"] == 0 || !rec.done["sidecars"] || rec.added["sidecars"] != rec.total["sidecars"] {
		t.Errorf("sidecars stage: %+v %+v %+v", rec.starts, rec.total, rec.added)
	}
}

// decide shows its sidecars and moves; a pass with nothing to move shows no stage.
func TestDecideShowsProgress(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, WriteXMP: true, Sort: true, UI: rec}, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec.finished(t, "sidecars", 3)
	rec.finished(t, "sort", 3)
	if rec.starts["home"] != 0 {
		t.Errorf("a home pass with nothing to move showed a stage")
	}
	rec = newRecorder()
	if _, err := Restore(c.ReportPath, dir, io.Discard, rec); err != nil {
		t.Fatal(err)
	}
	rec.finished(t, "restore", 3)
}
