package pipeline

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
)

// note reports a line at level l: through UI when set, otherwise to Log at the
// Normal level only (what a run without a sink printed before levels existed).
func (cfg Config) note(l ui.Level, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	switch {
	case cfg.UI != nil:
		cfg.UI.Emit(ui.Event{Note: &ui.Note{Level: l, Text: text}})
	case l <= ui.Normal && cfg.Log != nil:
		fmt.Fprintln(cfg.Log, text)
	}
}

// warn reports a problem that shows at every level.
func (cfg Config) warn(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	switch {
	case cfg.UI != nil:
		cfg.UI.Emit(ui.Event{Note: &ui.Note{Level: ui.Quiet, Sev: ui.Warn, Text: text}})
	case cfg.Log != nil:
		fmt.Fprintln(cfg.Log, text)
	}
}

// warnWriter is a writer for call sites that print problems line by line (moves):
// with a sink, each line is a warning, shown at every level.
func (cfg Config) warnWriter() io.Writer {
	if cfg.UI != nil {
		return ui.LineWriter(cfg.UI, ui.Quiet, ui.Warn)
	}
	return cfg.Log
}

// stage reports stage progress; only a sink renders it.
func (cfg Config) stage(s ui.Stage) {
	if cfg.UI != nil {
		cfg.UI.Emit(ui.Event{Stage: &s})
	}
}

// frame reports one finished frame: its "[n/total] …" line, plus (verbose) where
// focus was judged and what the calls cost.
func (cfg Config) frame(name string, n, total int, r report.Result) {
	line := fmt.Sprintf("[%d/%d] %s", n, total, summarize(r))
	if cfg.UI == nil {
		if cfg.Log != nil {
			fmt.Fprintln(cfg.Log, line)
		}
		return
	}
	cfg.stage(ui.Stage{Name: name, Add: 1})
	cfg.frameEvent(line, r)
}

// frameEvent reports a finished frame's line and tallies (the sink must be set).
func (cfg Config) frameEvent(line string, r report.Result) {
	if cfg.UI == nil {
		if cfg.Log != nil {
			fmt.Fprintln(cfg.Log, line)
		}
		return
	}
	decision := string(r.Decision)
	if r.Error != "" {
		decision = "error"
	}
	cfg.UI.Emit(ui.Event{Frame: &ui.Frame{Line: line, Decision: decision, CostUSD: r.CostUSD}})
	cfg.note(ui.Verbose, "  %s focus: %s; tokens in=%d out=%d; $%.4f",
		filepath.Base(r.File), focusDetail(r.FocusTarget), r.Usage.TotalIn(), r.Usage.OutputTokens, r.CostUSD)
}

// focusDetail is focusBrief with the measurements the verbose level adds.
func focusDetail(ft *report.FocusTarget) string {
	if ft == nil {
		return "none measured"
	}
	s := focusBrief(ft)
	if ft.SubjectSharpness > 0 {
		s += fmt.Sprintf(" subject %.3f", ft.SubjectSharpness)
	}
	return s + fmt.Sprintf(" landed %.3f, %d confident face(s)", ft.LandedSharpness, ft.Faces)
}

// stageName is what Run's progress is called: a scan measures, judge calls the model.
func (cfg Config) stageName() string {
	if cfg.DryRun {
		return "scan"
	}
	return "judge"
}
