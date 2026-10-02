package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// NewLive renders events as a live view on w, an interactive terminal: a progress
// bar per stage with an ETA, tallies, spend and sticky warnings. Frame lines and
// notes print above the view, so the scrollback keeps them as plain output would.
//
// The program never reads input and installs no signal handler: the terminal stays
// out of raw mode, so Ctrl-C still reaches the command's own handler (finish
// in-flight work, save, exit).
func NewLive(w io.Writer, l Level) (Sink, error) {
	m := newLiveModel(l, time.Now)
	p := tea.NewProgram(m, tea.WithOutput(w), tea.WithInput(nil), tea.WithoutSignalHandler())
	s := &live{p: p, level: l, done: make(chan struct{}), fallback: NewPlain(w, l)}
	go func() {
		_, s.err = p.Run()
		close(s.done)
	}()
	return s, nil
}

type live struct {
	p        *tea.Program
	level    Level
	done     chan struct{}
	err      error
	once     sync.Once
	fallback Sink // if the program stopped (it failed to start): plain lines
}

func (s *live) Emit(e Event) {
	select {
	case <-s.done:
		s.fallback.Emit(e)
		return
	default:
	}
	s.send(e)
}

// send hands e to the program. It never blocks once the program has stopped:
// Bubble Tea's Program.Println is a bare channel send, so lines go through Send,
// which gives up when the program's context ends.
func (s *live) send(e Event) {
	switch {
	case e.Note != nil:
		if e.Note.Sev == Info && e.Note.Level > s.level {
			return
		}
		s.p.Send(tea.Println(prefixed(e.Note))())
		if e.Note.Sev >= Warn {
			s.p.Send(eventMsg(e))
		}
	case e.Frame != nil:
		if s.level >= Normal {
			s.p.Send(tea.Println(e.Frame.Line)())
		}
		s.p.Send(eventMsg(e))
	case e.Stage != nil:
		s.p.Send(eventMsg(e))
	}
}

// Close stops the program after every event sent so far (they share its queue, so
// none is dropped) and leaves the last view on screen.
func (s *live) Close() error {
	s.once.Do(func() {
		s.p.Quit()
		<-s.done
	})
	return s.err
}

type eventMsg Event

type tickMsg struct{}

// refresh redraws the ETA between events.
func refresh() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

type stageState struct {
	name, unit  string
	total, done int64
	start       time.Time
	finished    bool
}

// tallyOrder is the order tallies show in; others follow alphabetically.
var tallyOrder = []string{"keep", "review", "cull", "junk", "error"}

type liveModel struct {
	level    Level
	now      func() time.Time
	width    int
	stages   []*stageState
	tally    map[string]int
	cost     float64
	warnings []string
	bar      progress.Model
}

func newLiveModel(l Level, now func() time.Time) *liveModel {
	return &liveModel{level: l, now: now, width: 80, tally: map[string]int{},
		bar: progress.New(progress.WithWidth(30), progress.WithoutPercentage(), progress.WithDefaultBlend())}
}

func (m *liveModel) Init() tea.Cmd { return refresh() }

func (m *liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tickMsg:
		return m, refresh()
	case eventMsg:
		m.apply(Event(msg))
	}
	return m, nil
}

func (m *liveModel) stage(name string) *stageState {
	for _, s := range m.stages {
		if s.name == name {
			return s
		}
	}
	s := &stageState{name: name, start: m.now()}
	m.stages = append(m.stages, s)
	return s
}

func (m *liveModel) apply(e Event) {
	switch {
	case e.Stage != nil:
		st := m.stage(e.Stage.Name)
		if e.Stage.Total > 0 && e.Stage.Add == 0 && !e.Stage.Done {
			st.total, st.unit, st.start, st.finished = e.Stage.Total, e.Stage.Unit, m.now(), false
		}
		st.done += e.Stage.Add
		if e.Stage.Done {
			st.finished = true
		}
	case e.Frame != nil:
		if e.Frame.Decision != "" {
			m.tally[e.Frame.Decision]++
		}
		m.cost += e.Frame.CostUSD
	case e.Note != nil && e.Note.Sev >= Warn:
		m.warnings = append(m.warnings, prefixed(e.Note))
		if len(m.warnings) > 3 {
			m.warnings = m.warnings[len(m.warnings)-3:]
		}
	}
}

var (
	nameStyle = lipgloss.NewStyle().Bold(true)
	dimStyle  = lipgloss.NewStyle().Faint(true)
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func (m *liveModel) View() tea.View {
	var b strings.Builder
	for _, w := range m.warnings {
		b.WriteString(warnStyle.Render(w) + "\n")
	}
	for _, s := range m.stages {
		b.WriteString(m.stageLine(s) + "\n")
	}
	var parts []string
	seen := map[string]bool{}
	for _, k := range tallyOrder {
		seen[k] = true
		if n := m.tally[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
	}
	for k, n := range m.tally {
		if !seen[k] && n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
	}
	if m.cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", m.cost))
	}
	if len(parts) > 0 {
		b.WriteString(strings.Join(parts, dimStyle.Render(" · ")) + "\n")
	}
	return tea.NewView(b.String())
}

func (m *liveModel) stageLine(s *stageState) string {
	name := nameStyle.Render(fmt.Sprintf("%-8s", s.name))
	if s.total <= 0 {
		if s.finished {
			return fmt.Sprintf("%s done", name)
		}
		return fmt.Sprintf("%s %d %s", name, s.done, s.unit)
	}
	frac := min(1, float64(s.done)/float64(s.total))
	count := fmt.Sprintf("%d/%d %s", s.done, s.total, s.unit)
	if s.unit == "bytes" {
		count = fmt.Sprintf("%s/%s", humanBytes(s.done), humanBytes(s.total))
	}
	tail := ""
	switch {
	case s.finished && s.done < s.total:
		tail = "stopped"
	case s.finished:
		tail = "done"
	case s.done > 0:
		el := m.now().Sub(s.start)
		left := time.Duration(float64(el) / float64(s.done) * float64(s.total-s.done))
		tail = left.Round(time.Second).String() + " left"
		if s.unit == "bytes" && el > 0 {
			tail = fmt.Sprintf("%s/s, %s", humanBytes(int64(float64(s.done)/el.Seconds())), tail)
		}
	}
	return fmt.Sprintf("%s %s %s  %s", name, m.bar.ViewAs(frac), count, dimStyle.Render(tail))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
