package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// NewPlain renders events as lines on w, filtered by level: what scripts, pipes and
// logs get.
func NewPlain(w io.Writer, l Level) Sink {
	return &plain{w: w, l: l, done: map[string]int64{}}
}

type plain struct {
	mu   sync.Mutex
	w    io.Writer
	l    Level
	done map[string]int64 // progress per stage, for the "done" line
}

func (p *plain) Emit(e Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case e.Note != nil:
		n := e.Note
		if n.Sev == Info && n.Level > p.l {
			return
		}
		fmt.Fprintln(p.w, prefixed(n))
	case e.Frame != nil:
		if p.l >= Normal {
			fmt.Fprintln(p.w, e.Frame.Line)
		}
	case e.Stage != nil:
		s := e.Stage
		p.done[s.Name] += s.Add
		if p.l < Verbose {
			return
		}
		switch {
		case s.Done:
			fmt.Fprintf(p.w, "%s: done (%d)\n", s.Name, p.done[s.Name])
		case s.Add == 0 && s.Total > 0:
			fmt.Fprintf(p.w, "%s: %d %s\n", s.Name, s.Total, s.Unit)
		}
	}
}

func (p *plain) Close() error { return nil }

// prefixed adds "warning: " or "error: " unless the text already says so.
func prefixed(n *Note) string {
	var pre string
	switch n.Sev {
	case Warn:
		pre = "warning: "
	case Error:
		pre = "error: "
	default:
		return n.Text
	}
	if strings.HasPrefix(n.Text, pre) {
		return n.Text
	}
	return pre + n.Text
}
