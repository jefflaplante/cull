// Package ui is how commands report what they are doing: notes at a verbosity level,
// stage progress, and per-frame results. A Sink renders them, as plain lines (any
// output, scripts, logs) or as a live view on an interactive terminal.
package ui

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Level is how much a run prints.
type Level int

const (
	Quiet   Level = iota // warnings, errors and the final summary only
	Normal               // progress and a line per frame (the default)
	Verbose              // per-stage and per-call detail
	Debug                // backend events, request sizes, raw model answers
)

var levelNames = []string{"quiet", "normal", "verbose", "debug"}

func (l Level) String() string {
	if l >= 0 && int(l) < len(levelNames) {
		return levelNames[l]
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// ParseLevel reads a --log-level value.
func ParseLevel(s string) (Level, error) {
	for i, n := range levelNames {
		if s == n {
			return Level(i), nil
		}
	}
	return Normal, fmt.Errorf("unknown log level %q (want quiet, normal, verbose or debug)", s)
}

// Severity marks warnings and errors, which show at every level.
type Severity int

const (
	Info Severity = iota
	Warn
	Error
)

// Event is one thing a command reports. Exactly one of the pointer fields is set.
type Event struct {
	Note  *Note
	Stage *Stage
	Frame *Frame
}

// Note is a line of text at a level, or a warning or error.
type Note struct {
	Level Level
	Sev   Severity
	Text  string
}

// Stage is a stage starting (Total set, Add 0), advancing (Add > 0) or finishing (Done).
type Stage struct {
	Name  string // "judge", "scan", "rank", "offload"
	Unit  string // "frames", "sets", "bytes"
	Total int64  // 0 = unknown
	Add   int64
	Done  bool
}

// Frame is one frame's result: its summary line, and what it adds to the tallies.
type Frame struct {
	Line     string
	Decision string // keep, review, cull, "" (measured only) or "error"
	CostUSD  float64
}

// Sink renders events. Emit is safe from several goroutines.
type Sink interface {
	Emit(Event)
	Close() error // idempotent; flushes and releases the terminal
}

// LineWriter adapts io.Writer call sites: each complete line written becomes a Note at
// level l and severity sev. Close (it implements io.Closer) flushes a partial last line.
func LineWriter(s Sink, l Level, sev Severity) io.Writer {
	return &lineWriter{s: s, l: l, sev: sev}
}

type lineWriter struct {
	mu  sync.Mutex
	s   Sink
	l   Level
	sev Severity
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.s.Emit(Event{Note: &Note{Level: w.l, Sev: w.sev, Text: string(w.buf[:i])}})
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.s.Emit(Event{Note: &Note{Level: w.l, Sev: w.sev, Text: string(w.buf)}})
		w.buf = nil
	}
	return nil
}
