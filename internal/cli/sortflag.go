package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// sortMode is --sort: all (keep/, review/, cull/), culls (only cull/), or none.
// A bare --sort means all; true/false are accepted so a ~/.cull "sort = true" keeps
// working.
type sortMode string

const (
	sortNone  sortMode = ""
	sortAll   sortMode = "all"
	sortCulls sortMode = "culls"
)

func (m *sortMode) String() string { return string(*m) }
func (m *sortMode) Type() string   { return "all|culls" }
func (m *sortMode) Set(s string) error {
	switch s {
	case "all", "true":
		*m = sortAll
	case "culls":
		*m = sortCulls
	case "", "false":
		*m = sortNone
	default:
		// the spellings the old bool flag took (1, t, T, TRUE, True, 0, f, ...)
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("want all or culls")
		}
		if b {
			*m = sortAll
		} else {
			*m = sortNone
		}
	}
	return nil
}

// flags maps the mode onto the pipeline's two placements.
func (m sortMode) flags() (moveCulled, sort bool) { return m == sortCulls, m == sortAll }

func registerSort(f *pflag.FlagSet, m *sortMode, usage string) {
	f.Var(m, "sort", usage)
	f.Lookup("sort").NoOptDefVal = string(sortAll)
}

// sortArgs is cobra.ExactArgs(n) that catches `--sort culls <dir>`: without the "="
// the value is taken as an argument.
func sortArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("sort") && (len(args) == n || len(args) == n+1) {
			for _, a := range args {
				if a == "culls" || a == "all" {
					return fmt.Errorf("write --sort=%s (with \"=\"): a bare --sort means all, so %q was read as an argument", a, a)
				}
			}
		}
		return cobra.ExactArgs(n)(cmd, args)
	}
}

// resolveMoveCulled folds the deprecated --move-culled into the sort mode. The
// flag typed on the command line wins over one from ~/.cull; with neither
// typed, --sort wins over the alias.
func resolveMoveCulled(cmd *cobra.Command, moveCulled bool, m sortMode) sortMode {
	if !moveCulled {
		return m
	}
	f := cmd.Flags()
	if f.Changed("sort") || (!f.Changed("move-culled") && m != sortNone) {
		return m
	}
	return sortCulls
}
