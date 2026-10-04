package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A stage with no known total still shows it's alive: a spinner that turns between
// redraws, and the time it has been running.
func TestLiveModelIndeterminateStageSpinsAndCounts(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newLiveModel(Normal, func() time.Time { return now })
	feed(m, Event{Stage: &Stage{Name: "batch", Start: true}})
	now = now.Add(5 * time.Second)
	a := m.View().Content
	now = now.Add(120 * time.Millisecond)
	b := m.View().Content
	if a == b {
		t.Fatalf("the view didn't change between redraws:\n%s", a)
	}
	if !strings.Contains(a, "5s") {
		t.Fatalf("no elapsed time:\n%s", a)
	}
}

// A stage with a total shows elapsed time before its first item lands, instead of a
// frozen empty bar.
func TestLiveModelStageShowsElapsedBeforeFirstItem(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newLiveModel(Normal, func() time.Time { return now })
	feed(m, Event{Stage: &Stage{Name: "sidecars", Unit: "files", Total: 992}})
	now = now.Add(3 * time.Second)
	if v := m.View().Content; !strings.Contains(v, "0/992 files") || !strings.Contains(v, "3s") {
		t.Fatalf("view:\n%s", v)
	}
}

// Start resets a stage that ran before under the same name.
func TestLiveModelRestartedStage(t *testing.T) {
	m := newLiveModel(Normal, time.Now)
	feed(m,
		Event{Stage: &Stage{Name: "sort", Unit: "frames", Total: 3, Start: true}},
		Event{Stage: &Stage{Name: "sort", Add: 3}},
		Event{Stage: &Stage{Name: "sort", Done: true}},
		Event{Stage: &Stage{Name: "sort", Unit: "frames", Total: 5, Start: true}},
		Event{Stage: &Stage{Name: "sort", Add: 1}})
	if v := m.View().Content; !strings.Contains(v, "1/5 frames") || strings.Contains(v, "done") {
		t.Fatalf("view:\n%s", v)
	}
}

// Plain output announces a described stage at the normal level, so a log or a pipe
// says what a quiet step is doing; undescribed stages stay verbose-only.
func TestPlainAnnouncesDescribedStages(t *testing.T) {
	var b bytes.Buffer
	s := NewPlain(&b, Normal)
	tr := Track(s, "sidecars", "writing sidecars", "files", 3)
	tr.Add(3)
	tr.Done()
	Track(s, "apply-c1", "running the script in Capture One", "", 0).Done()
	if want := "writing sidecars: 3 files\nrunning the script in Capture One…\n"; b.String() != want {
		t.Fatalf("got %q want %q", b.String(), want)
	}
	b.Reset()
	NewPlain(&b, Quiet).Emit(Event{Stage: &Stage{Name: "x", Text: "doing x", Start: true}})
	if b.Len() != 0 {
		t.Fatalf("quiet printed %q", b.String())
	}
}

// A nil sink makes a nil tracker, which does nothing.
func TestTrackNilSink(t *testing.T) {
	tr := Track(nil, "sort", "sorting", "frames", 2)
	tr.Add(1)
	tr.Done()
}

// An open-ended stage shows what it is doing while it has nothing to count.
func TestLiveModelIndeterminateStageShowsText(t *testing.T) {
	m := newLiveModel(Normal, time.Now)
	feed(m, Event{Stage: &Stage{Name: "apply-c1", Text: "running the script in Capture One", Start: true}})
	if v := m.View().Content; !strings.Contains(v, "running the script in Capture One") {
		t.Fatalf("view:\n%s", v)
	}
}
