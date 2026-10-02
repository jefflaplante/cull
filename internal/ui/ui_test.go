package ui

import (
	"bytes"
	"io"
	"testing"
)

func emitAll(s Sink) {
	s.Emit(Event{Note: &Note{Level: Normal, Text: "normal note"}})
	s.Emit(Event{Note: &Note{Level: Verbose, Text: "verbose note"}})
	s.Emit(Event{Note: &Note{Level: Debug, Text: "debug note"}})
	s.Emit(Event{Note: &Note{Level: Debug, Sev: Warn, Text: "disk nearly full"}})
	s.Emit(Event{Frame: &Frame{Line: "L1.DNG keep 8.0", Decision: "keep"}})
}

func TestPlainLevels(t *testing.T) {
	cases := map[Level]string{
		Quiet:   "warning: disk nearly full\n",
		Normal:  "normal note\nwarning: disk nearly full\nL1.DNG keep 8.0\n",
		Verbose: "normal note\nverbose note\nwarning: disk nearly full\nL1.DNG keep 8.0\n",
		Debug:   "normal note\nverbose note\ndebug note\nwarning: disk nearly full\nL1.DNG keep 8.0\n",
	}
	for l, want := range cases {
		var b bytes.Buffer
		s := NewPlain(&b, l)
		emitAll(s)
		s.Close()
		if b.String() != want {
			t.Errorf("level %v:\ngot  %q\nwant %q", l, b.String(), want)
		}
	}
}

func TestPlainErrorPrefix(t *testing.T) {
	var b bytes.Buffer
	s := NewPlain(&b, Quiet)
	s.Emit(Event{Note: &Note{Sev: Error, Text: "boom"}})
	s.Emit(Event{Note: &Note{Sev: Warn, Text: "warning: already prefixed"}})
	if want := "error: boom\nwarning: already prefixed\n"; b.String() != want {
		t.Fatalf("got %q want %q", b.String(), want)
	}
}

func TestPlainStageLines(t *testing.T) {
	stages := func(s Sink) {
		s.Emit(Event{Stage: &Stage{Name: "judge", Unit: "frames", Total: 17}})
		s.Emit(Event{Stage: &Stage{Name: "judge", Add: 1}})
		s.Emit(Event{Stage: &Stage{Name: "judge", Done: true}})
	}
	var b bytes.Buffer
	stages(NewPlain(&b, Normal))
	if b.Len() != 0 {
		t.Fatalf("normal level printed stage lines: %q", b.String())
	}
	b.Reset()
	stages(NewPlain(&b, Verbose))
	if want := "judge: 17 frames\njudge: done (1)\n"; b.String() != want {
		t.Fatalf("got %q want %q", b.String(), want)
	}
}

func TestLineWriterSplitsLines(t *testing.T) {
	var b bytes.Buffer
	s := NewPlain(&b, Normal)
	w := LineWriter(s, Normal, Info)
	io.WriteString(w, "a\nb")
	io.WriteString(w, "c\n")
	io.WriteString(w, "tail")
	if b.String() != "a\nbc\n" {
		t.Fatalf("before close: %q", b.String())
	}
	w.(io.Closer).Close()
	if b.String() != "a\nbc\ntail\n" {
		t.Fatalf("after close: %q", b.String())
	}
}

func TestLineWriterRespectsLevel(t *testing.T) {
	var b bytes.Buffer
	w := LineWriter(NewPlain(&b, Normal), Verbose, Info)
	io.WriteString(w, "hidden\n")
	if b.Len() != 0 {
		t.Fatalf("verbose line shown at normal: %q", b.String())
	}
}

func TestParseLevel(t *testing.T) {
	for s, want := range map[string]Level{"quiet": Quiet, "normal": Normal, "verbose": Verbose, "debug": Debug} {
		got, err := ParseLevel(s)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("loud accepted")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := NewPlain(io.Discard, Normal)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
