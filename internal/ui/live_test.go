package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func feed(m *liveModel, events ...Event) {
	for _, e := range events {
		m.Update(eventMsg(e))
	}
}

func TestLiveModelShowsStageAndTallies(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newLiveModel(Normal, func() time.Time { return now })
	feed(m, Event{Stage: &Stage{Name: "judge", Unit: "frames", Total: 10}})
	now = now.Add(30 * time.Second)
	for _, d := range []string{"keep", "keep", "cull"} {
		feed(m,
			Event{Stage: &Stage{Name: "judge", Add: 1}},
			Event{Frame: &Frame{Line: "x", Decision: d, CostUSD: 0.01}})
	}
	v := m.View().Content
	for _, want := range []string{"judge", "3/10 frames", "keep 2", "cull 1", "$0.03", "1m10s left"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "review") {
		t.Errorf("view shows an empty tally:\n%s", v)
	}
}

func TestLiveModelKeepsWarningsVisible(t *testing.T) {
	m := newLiveModel(Normal, time.Now)
	feed(m, Event{Note: &Note{Sev: Warn, Text: "checkpoint failed: disk full"}})
	for i := 0; i < 20; i++ {
		feed(m, Event{Frame: &Frame{Line: "f", Decision: "keep"}})
	}
	if v := m.View().Content; !strings.Contains(v, "checkpoint failed: disk full") {
		t.Fatalf("warning scrolled away:\n%s", v)
	}
}

func TestLiveModelFinishedStage(t *testing.T) {
	m := newLiveModel(Normal, time.Now)
	feed(m,
		Event{Stage: &Stage{Name: "scan", Unit: "frames", Total: 2}},
		Event{Stage: &Stage{Name: "scan", Add: 2}},
		Event{Stage: &Stage{Name: "scan", Done: true}})
	if v := m.View().Content; !strings.Contains(v, "2/2 frames") || !strings.Contains(v, "done") {
		t.Fatalf("finished stage:\n%s", v)
	}
}

// The live sink, run against a plain buffer (no terminal), still prints every frame
// line and note into the output by the time Close returns, and Close is idempotent.
func TestLiveCloseFlushesLinesAndIsIdempotent(t *testing.T) {
	var b bytes.Buffer
	s, err := NewLive(&b, Normal)
	if err != nil {
		t.Fatal(err)
	}
	s.Emit(Event{Stage: &Stage{Name: "judge", Unit: "frames", Total: 2}})
	s.Emit(Event{Frame: &Frame{Line: "[1/2] L1.DNG KEEP", Decision: "keep"}})
	s.Emit(Event{Note: &Note{Level: Verbose, Text: "hidden verbose"}})
	s.Emit(Event{Frame: &Frame{Line: "[2/2] L2.DNG CULL", Decision: "cull"}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "[1/2] L1.DNG KEEP") || !strings.Contains(out, "[2/2] L2.DNG CULL") {
		t.Fatalf("frame lines lost:\n%q", out)
	}
	if strings.Contains(out, "hidden verbose") {
		t.Fatalf("verbose note shown at normal:\n%q", out)
	}
}

func TestLiveModelStoppedStage(t *testing.T) {
	m := newLiveModel(Normal, time.Now)
	feed(m,
		Event{Stage: &Stage{Name: "scan", Unit: "frames", Total: 17}},
		Event{Stage: &Stage{Name: "scan", Add: 8}},
		Event{Stage: &Stage{Name: "scan", Done: true}})
	if v := m.View().Content; !strings.Contains(v, "8/17 frames") || !strings.Contains(v, "stopped") || strings.Contains(v, "done") {
		t.Fatalf("interrupted stage:\n%s", v)
	}
}
