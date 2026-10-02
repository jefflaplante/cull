package cli

import (
	"errors"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/ui"
)

// outputOpts are the verbosity flags every command takes.
type outputOpts struct {
	quiet, verbose, debug, plain bool
	logLevel                     string
}

func (o *outputOpts) register(pf *pflag.FlagSet) {
	pf.BoolVarP(&o.quiet, "quiet", "q", false, "print only warnings, errors and the final summary")
	pf.BoolVarP(&o.verbose, "verbose", "v", false, "also print per-stage and per-call detail (focus target, tokens, cost)")
	pf.BoolVar(&o.debug, "debug", false, "also print backend events, request sizes and raw model answers (never credentials)")
	pf.StringVar(&o.logLevel, "log-level", "", "quiet, normal, verbose or debug (the long form of -q / -v / --debug)")
	pf.BoolVar(&o.plain, "plain", false, "plain lines even on a terminal (no live progress view)")
}

// level resolves the verbosity flags; it refuses contradictory ones.
func (o *outputOpts) level() (ui.Level, error) {
	n, l := 0, ui.Normal
	for _, f := range []struct {
		on bool
		l  ui.Level
	}{{o.quiet, ui.Quiet}, {o.verbose, ui.Verbose}, {o.debug, ui.Debug}} {
		if f.on {
			n, l = n+1, f.l
		}
	}
	if o.logLevel != "" {
		if n > 0 {
			return l, errors.New("--log-level replaces -q, -v and --debug: pick one")
		}
		return ui.ParseLevel(o.logLevel)
	}
	if n > 1 {
		return l, errors.New("pick one of -q, -v and --debug")
	}
	return l, nil
}

// output is one command's reporting: a sink for events, and a level-filtered writer
// (Normal notes) for call sites that print lines.
type output struct {
	UI     ui.Sink
	Log    io.Writer
	closed bool
}

// newOutput builds the command's output. live asks for the live terminal view where
// it is allowed (long-running commands only).
func (o *outputOpts) newOutput(cmd *cobra.Command, live bool) *output {
	l, _ := o.level() // validated in the root's PersistentPreRunE
	s := ui.NewPlain(cmd.ErrOrStderr(), l)
	return &output{UI: s, Log: ui.LineWriter(s, ui.Normal, ui.Info)}
}

// attach routes a pipeline run's reporting through this output.
func (o *output) attach(cfg *pipeline.Config) { cfg.UI, cfg.Log = o.UI, o.Log }

// Close flushes and releases the output; call it before printing the final summary.
func (o *output) Close() {
	if o.closed {
		return
	}
	o.closed = true
	o.Log.(io.Closer).Close()
	o.UI.Close()
}
