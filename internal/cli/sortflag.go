package cli

import (
	"fmt"

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
		return fmt.Errorf("want all or culls")
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
		if len(args) == n+1 && cmd.Flags().Changed("sort") && (args[0] == "culls" || args[0] == "all") {
			return fmt.Errorf("write --sort=%s (with \"=\"): a bare --sort means all, so %q was read as the folder", args[0], args[0])
		}
		return cobra.ExactArgs(n)(cmd, args)
	}
}
