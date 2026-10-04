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
		starting := s.Start || (s.Add == 0 && s.Total > 0 && !s.Done)
		if starting {
			p.done[s.Name] = 0
		}
		p.done[s.Name] += s.Add
		// A described stage says what it is doing at the normal level: a step that
		// prints nothing per item would otherwise look stalled in a log or a pipe.
		if starting && s.Text != "" && p.l >= Normal {
			if s.Total > 0 {
				fmt.Fprintf(p.w, "%s: %d %s\n", s.Text, s.Total, s.Unit)
			} else {
				fmt.Fprintf(p.w, "%s…\n", s.Text)
			}
			return
		}
		if p.l < Verbose {
			return
		}
		switch {
		case s.Done:
			fmt.Fprintf(p.w, "%s: done (%d)\n", s.Name, p.done[s.Name])
		case starting && s.Total > 0:
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
