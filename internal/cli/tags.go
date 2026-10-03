package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/report"
)

// tagFlags are the shoot keywords offload, scan and judge take.
type tagFlags struct {
	project, event, location string
	keywords                 []string
}

func (t *tagFlags) register(f *pflag.FlagSet) {
	f.StringVar(&t.project, "project", "", "project keyword for every frame (stored: later runs keep it)")
	f.StringVar(&t.event, "event", "", "event keyword for every frame")
	f.StringVar(&t.location, "location", "", "location keyword for every frame, e.g. \"Forest Park, Portland\"")
	f.StringArrayVar(&t.keywords, "keyword", nil, "an extra keyword for every frame (repeat for more; replaces the stored list)")
}

// tags is what was given on the command line (nil when nothing was).
func (t *tagFlags) tags() (*report.Tags, error) {
	out := &report.Tags{Project: t.project, Event: t.event, Location: t.location}
	for _, k := range t.keywords {
		if k = strings.TrimSpace(k); k != "" {
			out.Keywords = append(out.Keywords, k)
		}
	}
	out.Project, out.Event, out.Location = strings.TrimSpace(out.Project), strings.TrimSpace(out.Event), strings.TrimSpace(out.Location)
	for _, v := range append([]string{out.Project, out.Event, out.Location}, out.Keywords...) {
		if strings.Contains(v, "|") {
			return nil, fmt.Errorf("tag %q: \"|\" separates keyword levels, so it can't be part of a value", v)
		}
		if strings.HasPrefix(strings.ToLower(v), "cull:") {
			return nil, fmt.Errorf("tag %q: cull: keywords are cull's own verdict markers", v)
		}
	}
	if out.Project == "" && out.Event == "" && out.Location == "" && len(out.Keywords) == 0 {
		return nil, nil
	}
	return out, nil
}

func newTagCmd(so *sharedOpts) *cobra.Command {
	var tf tagFlags
	var clearProject, clearEvent, clearLocation, clearKeywords bool
	cmd := &cobra.Command{
		Use:   "tag <dir>",
		Short: "Show or change the shoot's keywords (project, event, location, extra keywords)",
		Long: `tag shows the keywords stored in the folder's report, or changes them. Every
frame's sidecar carries them: run 'cull decide --write-xmp <dir>' afterwards to
rewrite the sidecars, and 'cull apply-c1' for frames already in Capture One.`,
		Example: `  cull tag ~/Pictures/"2026-10-02 Smith wedding"
  cull tag --event Ceremony --clear-location ~/Pictures/"2026-10-02 Smith wedding"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return fmt.Errorf("no report at %s: scan or judge the folder first (%w)", cfg.ReportPath, err)
			}
			given, err := tf.tags()
			if err != nil {
				return err
			}
			changed := given != nil || clearProject || clearEvent || clearLocation || clearKeywords
			t := report.MergeTags(rep.Tags, given)
			if t != nil {
				c := *t
				if clearProject {
					c.Project = ""
				}
				if clearEvent {
					c.Event = ""
				}
				if clearLocation {
					c.Location = ""
				}
				if clearKeywords {
					c.Keywords = nil
				}
				t = report.MergeTags(&c, nil)
			}
			w := cmd.ErrOrStderr()
			if changed {
				rep.Tags = t
				if err := rep.Save(cfg.ReportPath); err != nil {
					return err
				}
			}
			if t == nil {
				fmt.Fprintln(w, "no tags")
			} else {
				for _, f := range []struct{ k, v string }{{"project", t.Project}, {"event", t.Event}, {"location", t.Location}, {"keywords", strings.Join(t.Keywords, ", ")}} {
					if f.v != "" {
						fmt.Fprintf(w, "%s: %s\n", f.k, f.v)
					}
				}
			}
			if changed {
				fmt.Fprintf(w, "saved; rewrite the sidecars with: cull decide --write-xmp %q\n", args[0])
			}
			return nil
		},
	}
	tf.register(cmd.Flags())
	f := cmd.Flags()
	f.BoolVar(&clearProject, "clear-project", false, "remove the project")
	f.BoolVar(&clearEvent, "clear-event", false, "remove the event")
	f.BoolVar(&clearLocation, "clear-location", false, "remove the location")
	f.BoolVar(&clearKeywords, "clear-keywords", false, "remove the extra keywords")
	return cmd
}
