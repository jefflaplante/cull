package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Flag sections: help prints each command's flags grouped, the everyday ones first.
// Tuning and Experimental appear only under --help-all; every flag still works and
// completes either way.
const (
	sectionKey      = "cull_section"
	secCommon       = ""
	secPolicy       = "Policy"
	secBackend      = "Backend"
	secSidecars     = "Sidecars & folders"
	secTuning       = "Tuning"
	secExperimental = "Experimental"
)

var sectionOrder = []struct{ sec, title string }{
	{secCommon, "Flags:"},
	{secPolicy, "Policy flags (stored in the report; tune them later with cull decide, free):"},
	{secBackend, "Backend flags:"},
	{secSidecars, "Sidecar and folder flags:"},
	{secTuning, "Tuning flags:"},
	{secExperimental, "Experimental flags (not yet measured against labels):"},
}

// folded are the sections plain --help leaves out.
var folded = map[string]bool{secTuning: true, secExperimental: true}

// showAllFlags is set by --help-all for the help being printed.
var showAllFlags bool

// setSection puts the named flags of fs in sec. An unknown name panics: it is a typo,
// and every test that builds the command tree catches it.
func setSection(fs *pflag.FlagSet, sec string, names ...string) {
	for _, n := range names {
		if err := fs.SetAnnotation(n, sectionKey, []string{sec}); err != nil {
			panic(fmt.Sprintf("setSection %q: %v", n, err))
		}
	}
}

func sectionOf(f *pflag.Flag) string {
	if v := f.Annotations[sectionKey]; len(v) == 1 {
		return v[0]
	}
	return secCommon
}

// sectionedFlags renders c's own flags by section, for the usage template.
func sectionedFlags(c *cobra.Command) string {
	sets := map[string]*pflag.FlagSet{}
	hidden := 0
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" {
			return
		}
		s := sectionOf(f)
		if folded[s] && !showAllFlags {
			hidden++
			return
		}
		if sets[s] == nil {
			sets[s] = pflag.NewFlagSet(s, pflag.ContinueOnError)
		}
		sets[s].AddFlag(f)
	})
	var b strings.Builder
	for _, s := range sectionOrder {
		fs := sets[s.sec]
		if fs == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.title + "\n" + strings.TrimRight(fs.FlagUsages(), " \n"))
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n\n%d more flags (tuning, experimental): %s --help-all", hidden, c.CommandPath())
	}
	return b.String()
}

// helpAllValue is --help-all: it turns on the help flag too, so cobra prints help
// before checking arguments (a bare `cull judge --help-all` needs no folder).
type helpAllValue struct{ all, help *bool }

func (v helpAllValue) String() string   { return fmt.Sprint(*v.all) }
func (v helpAllValue) Type() string     { return "bool" }
func (v helpAllValue) IsBoolFlag() bool { return true }
func (v helpAllValue) Set(s string) error {
	on := s == "true"
	if !on && s != "false" {
		return fmt.Errorf("want true or false")
	}
	*v.all, *v.help = on, on
	return nil
}

// installHelp adds -h/--help and --help-all to root and swaps the usage template's
// flag list for the sectioned one.
func installHelp(root *cobra.Command) {
	showAllFlags = false
	var help bool
	pf := root.PersistentFlags()
	pf.BoolVarP(&help, "help", "h", false, "help for this command")
	pf.Var(helpAllValue{all: &showAllFlags, help: &help}, "help-all", "help listing every flag, tuning and experimental ones too")
	pf.Lookup("help-all").NoOptDefVal = "true"
	cobra.AddTemplateFunc("sectionedFlags", sectionedFlags)
	const plain = "Flags:\n{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}"
	t := root.UsageTemplate()
	if !strings.Contains(t, plain) {
		panic("cobra's usage template changed: update installHelp")
	}
	root.SetUsageTemplate(strings.Replace(t, plain, "{{sectionedFlags .}}", 1))
}
