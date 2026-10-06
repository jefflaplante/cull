package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Plain --help shows the everyday sections in order and folds tuning and
// experimental flags into a count; --help-all shows them.
func TestJudgeHelpSections(t *testing.T) {
	out, err := run(t, "judge", "--help")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"Flags:", "Policy flags", "Backend flags", "Sidecar and folder flags", "Global Flags:"}
	at := -1
	for _, h := range order {
		i := strings.Index(out, h)
		if i < 0 || i < at {
			t.Fatalf("section %q missing or out of order in:\n%s", h, out)
		}
		at = i
	}
	for _, hidden := range []string{"--second-opinion", "--rank-twice", "--escalate-backend", "--checkpoint", "Tuning flags", "Experimental flags"} {
		if strings.Contains(out, hidden) {
			t.Errorf("plain --help shows %s", hidden)
		}
	}
	if !strings.Contains(out, "more flags (tuning, experimental): cull judge --help-all") {
		t.Errorf("no --help-all pointer:\n%s", out)
	}
	all, err := run(t, "judge", "--help-all")
	if err != nil {
		t.Fatalf("--help-all without a folder must just print help: %v", err)
	}
	for _, want := range []string{"Tuning flags", "Experimental flags", "--second-opinion", "--checkpoint"} {
		if !strings.Contains(all, want) {
			t.Errorf("--help-all lacks %s", want)
		}
	}
	if _, err := run(t, "--help-all"); err != nil {
		t.Fatalf("root --help-all: %v", err)
	}
}

// Every flag on every command is in a known section, so a new flag can't land
// somewhere the template doesn't print.
func TestEveryFlagHasAKnownSection(t *testing.T) {
	known := map[string]bool{secCommon: true, secPolicy: true, secBackend: true, secSidecars: true, secTuning: true, secExperimental: true}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if !known[sectionOf(f)] {
				t.Errorf("%s --%s: section %q", c.CommandPath(), f.Name, sectionOf(f))
			}
		})
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(NewRootCmd())
}

// No help text of a current command teaches a retired form.
func TestHelpTeachesNoRetiredForms(t *testing.T) {
	retired := []string{"--move-culled", "--write-xmp", "--resume", "--xmp-develop", "cull rank", "import-labels", "--static", "culled/"}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Deprecated == "" && !c.Hidden {
			out, err := run(t, append(strings.Fields(strings.TrimPrefix(c.CommandPath(), "cull")), "--help-all")...)
			if err != nil {
				t.Fatalf("%s: %v", c.CommandPath(), err)
			}
			for _, r := range retired {
				if strings.Contains(out, r) {
					t.Errorf("%s --help-all mentions %s", c.CommandPath(), r)
				}
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(NewRootCmd())
}
