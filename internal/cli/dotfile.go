package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/config"
)

// notInDotfile are flags a dotfile may not default: one-off actions, ones that spend
// money, overwrite or skip a safety check without asking, and the verbosity flags
// (a dotfile -q would clash with a typed -v). Type them when you mean them.
var notInDotfile = map[string]bool{
	"yes": true, "fresh": true, "force": true, "run": true, "probe": true, "verify": true,
	"dry-run": true, "estimate": true, "prepare": true, "clear-cache": true, "resume": true,
	"overwrite-xmp": true, "report": true, "help": true, "version": true,
	"quiet": true, "verbose": true, "debug": true, "log-level": true,
}

// applyDotfile sets flag defaults from the user's dotfile (config.SettingsPath) on
// cmd, after its flags are parsed and before it runs. A flag typed on the command
// line wins; a dotfile value is not "typed", so a report's stored policy (decide,
// rank, judge --resume) still wins over it: the dotfile replaces only the built-in
// defaults. Lines without a section apply to every command with the flag; a
// [command] section applies to that command, after the lines without one. It notes
// the values it used on w unless quiet.
func applyDotfile(cmd *cobra.Command, w io.Writer, quiet bool) error {
	if name := cmd.Name(); strings.HasPrefix(name, "__") || name == "completion" || name == "help" {
		return nil
	}
	s, err := config.LoadSettings()
	if err != nil || len(s.Settings) == 0 {
		return err
	}
	known := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			fs.VisitAll(func(f *pflag.Flag) { known[f.Name] = true })
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd.Root())
	var used []string
	seen := map[string]bool{}
	for _, global := range []bool{true, false} {
		for _, st := range s.Settings {
			if global != (st.Section == "") || (!global && st.Section != cmd.Name()) {
				continue
			}
			at := fmt.Sprintf("%s:%d: %s", s.Path, st.Line, st.Key)
			switch {
			case notInDotfile[st.Key]:
				fmt.Fprintf(w, "warning: %s: not allowed in the dotfile (type it when you mean it)\n", at)
				continue
			case !known[st.Key]:
				fmt.Fprintf(w, "warning: %s: no command has this flag\n", at)
				continue
			}
			f := cmd.Flags().Lookup(st.Key)
			if f == nil || f.Changed {
				continue // another command's flag, or typed on the command line
			}
			if err := f.Value.Set(st.Value); err != nil {
				return fmt.Errorf("%s: %v", at, err)
			}
			if !seen[st.Key] || f.Value.Type() == "stringArray" || f.Value.Type() == "stringSlice" {
				used = append(used, "--"+st.Key+" "+st.Value)
			}
			seen[st.Key] = true
		}
	}
	if len(used) > 0 && !quiet {
		fmt.Fprintf(w, "defaults from %s: %s\n", s.Path, strings.Join(used, " "))
	}
	return nil
}
