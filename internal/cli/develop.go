package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/develop"
	"github.com/jefflaplante/cull/internal/report"
)

func newDevelopCmd(so *sharedOpts) *cobra.Command {
	var (
		out, lc, wb, labelsPath               string
		jobs, chunk, longEdge, quality        int
		noStraighten, noPreset, force, dryRun bool
		yes, noLabels                         bool
	)
	cmd := &cobra.Command{
		Use:   "develop <dir>",
		Short: "Develop the keeps into client JPEGs with LightCraft (hours of CPU: it asks first)",
		Long: `develop renders every keep (your label, else the model's verdict) into a JPEG with
LightCraft's headless lightcraft-cli. cull stays the decision layer: LightCraft reads
each keep's sidecar (exposure and crop) on import, and does all the rendering.

The recipe, per frame:
  develop.wb mode=auto          (--wb)
  crop.autoStraighten           levels it; cull's crop stands (--no-straighten)
  preset.apply                  the camera's preset: LEICA M10-R → leica-m10r-std;
                                another body gets develop.auto instead, with a warning
  develop.set light.exposure    cull's EV again, on top of the preset's own exposure:
                                the auto stages and the preset overwrite the sidecar's
  app.export                    JPEG, 3000 px long edge, quality 95 (--long-edge, --quality)

LightCraft takes about 25 s and 2.9 GB of memory per 60 MP frame, CPU only: a
300-frame shoot is an overnight run. develop prints its estimate and asks before it
starts (--yes to skip that; without a terminal it needs --yes). -j runs more
LightCraft processes at once, each with its own ~2.9 GB.

The JPEGs go to <dir>/export (--out), named after the frames. cull-develop.json
records the recipe and each export, saved after every frame: an interrupted run
(Ctrl-C, a crash, a sleeping laptop) loses only the frames in progress, and a re-run
develops only what is missing, failed or changed (the sidecar, the DNG, the recipe).
A JPEG cull didn't export is never replaced.`,
		Example: `  cull develop --dry-run ~/Pictures/2026-10-04
  cull develop ~/Pictures/2026-10-04
  cull develop --yes --out /Volumes/Delivery/Smith ~/Pictures/2026-10-04`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.ErrOrStderr()
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			r := develop.DefaultRecipe()
			r.WB, r.Straighten, r.Presets = wb, !noStraighten, !noPreset
			if wb == "none" {
				r.WB = ""
			}
			r.Export.LongEdge, r.Export.Quality = longEdge, quality
			if err := r.Validate(); err != nil {
				return err
			}
			if lc == "" {
				if lc, err = exec.LookPath("lightcraft-cli"); err != nil && !dryRun {
					return errors.New("lightcraft-cli isn't on your PATH: install LightCraft, or pass --lightcraft <path to lightcraft-cli>")
				}
			}
			if out == "" {
				out = filepath.Join(cfg.Dir, "export")
			}
			if out, err = filepath.Abs(out); err != nil {
				return err
			}
			release, err := holdShoot(cmd, cfg.Dir, so.recursive, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			lab, err := userLabels(w, cfg.ReportPath, labelsPath, noLabels)
			if err != nil {
				return err
			}
			var keeps []develop.Keep
			rep, err := report.Load(cfg.ReportPath)
			switch {
			case err == nil:
				keeps = develop.FromReport(rep.Results, lab)
			case !errors.Is(err, fs.ErrNotExist):
				return err
			case len(lab) == 0:
				return fmt.Errorf("no report (%s) and no labels: develop renders the keeps, so judge the shoot or label it in cull review first", cfg.ReportPath)
			default:
				var missing []string
				if keeps, missing, err = develop.FromLabels(cfg.Dir, so.recursive, lab); err != nil {
					return err
				}
				for _, m := range missing {
					fmt.Fprintf(w, "warning: %s is labelled keep but isn't in %s (-r searches subfolders)\n", m, cfg.Dir)
				}
			}
			if len(keeps) == 0 {
				fmt.Fprintln(w, "no keeps to develop")
				return nil
			}
			base := filepath.Dir(cfg.ReportPath)
			opts := develop.Options{
				StatePath: filepath.Join(base, develop.StateName), Work: filepath.Join(base, develop.WorkName),
				Out: out, LightCraft: lc, Recipe: r, Jobs: jobs, Chunk: chunk, Force: force,
			}
			plan, err := develop.Prepare(opts, keeps)
			if err != nil {
				return err
			}
			describePlan(w, len(keeps), out, r, plan)
			if len(plan.Frames) == 0 {
				return failures(w, develop.Summary{Failures: plan.Unreadable})
			}
			if dryRun {
				fmt.Fprintln(w, "dry run: nothing developed")
				return nil
			}
			if err := confirmDevelop(cmd, plan.Estimate, len(plan.Frames), yes); err != nil {
				return err
			}
			o := so.out.newOutput(cmd, false)
			opts.Log = o.Log
			sum, err := develop.Run(cmd.Context(), opts, plan)
			o.Close()
			describeRun(w, out, sum)
			if err != nil {
				if errors.Is(err, cmd.Context().Err()) {
					return fmt.Errorf("interrupted: run cull develop again to continue (%d developed so far)", sum.Developed)
				}
				return err
			}
			return failures(w, sum)
		},
	}
	f := cmd.Flags()
	f.StringVar(&out, "out", "", "the export folder (default <dir>/export)")
	f.StringVar(&lc, "lightcraft", "", "lightcraft-cli to run (default: the one on your PATH)")
	f.IntVarP(&jobs, "concurrency", "j", 1, "LightCraft processes at once (~2.9 GB RAM each for 60 MP frames)")
	f.BoolVar(&yes, "yes", false, "start without asking (develop asks on a terminal; without one it needs --yes)")
	f.BoolVar(&dryRun, "dry-run", false, "print the keeps, the recipe and the estimate; develop nothing")
	f.BoolVar(&force, "force", false, "develop up-to-date keeps again too")
	f.StringVar(&wb, "wb", "auto", "white balance for every frame: auto, asShot, daylight, cloudy, shade, tungsten, fluorescent, flash, or none (the file's)")
	f.BoolVar(&noStraighten, "no-straighten", false, "don't level horizons (crop.autoStraighten)")
	f.BoolVar(&noPreset, "no-preset", false, "no camera presets: develop.auto on every frame")
	f.IntVar(&longEdge, "long-edge", 3000, "the JPEGs' long edge in pixels (0 = full size)")
	f.IntVar(&quality, "quality", 95, "JPEG quality, 1-100")
	f.IntVar(&chunk, "chunk", 10, "frames per LightCraft process: a crash or Ctrl-C costs at most the frame in progress either way")
	f.StringVar(&labelsPath, "labels", "", "your labels log (default: cull-labels.jsonl beside the report, when it exists)")
	f.BoolVar(&noLabels, "no-labels", false, "ignore your labels: develop the model's keeps")
	setSection(f, secSidecars, "labels", "no-labels")
	setSection(f, secTuning, "chunk")
	return cmd
}

// describePlan prints what develop will do: the keeps, the recipe, presets per camera,
// and the cost.
func describePlan(w io.Writer, keeps int, out string, r develop.Recipe, p *develop.Plan) {
	fmt.Fprintf(w, "%d keep(s): %d up to date, %d to develop → %s\n", keeps, p.UpToDate, len(p.Frames), out)
	var stages []string
	if r.WB != "" {
		stages = append(stages, "wb "+r.WB)
	}
	if r.Straighten {
		stages = append(stages, "auto-straighten")
	}
	if r.Presets {
		stages = append(stages, "camera preset (else develop.auto)")
	} else {
		stages = append(stages, "develop.auto")
	}
	stages = append(stages, "your sidecar's EV and crop")
	size := fmt.Sprintf("%d px", r.Export.LongEdge)
	if r.Export.LongEdge == 0 {
		size = "full size"
	}
	fmt.Fprintf(w, "recipe: %s; JPEG %s, quality %d\n", strings.Join(stages, ", "), size, r.Export.Quality)
	cams := map[string]int{}
	for _, f := range p.Frames {
		if pr, ok := develop.PresetFor(f.Model); ok && r.Presets {
			cams[strings.TrimSpace(f.Model)+" → "+pr.ID]++
		}
	}
	var lines []string
	for c, n := range cams {
		lines = append(lines, fmt.Sprintf("  %s: %d frame(s)", c, n))
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	var models []string
	for m := range p.NoPreset {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		fmt.Fprintf(w, "warning: no LightCraft preset for %s (%d frame(s)): develop.auto instead\n", m, p.NoPreset[m])
	}
	for _, u := range p.Unreadable {
		fmt.Fprintf(w, "warning: %s can't be developed: %s\n", u.Name, u.Err)
	}
	if len(p.Frames) == 0 {
		return
	}
	e := p.Estimate
	src := "LightCraft's ~25 s per 60 MP frame"
	if e.Measured {
		src = "this shoot's measured time"
	}
	fmt.Fprintf(w, "estimate: ~%s for %d frame(s) (%s per frame, %s), %d LightCraft process(es), peak memory ~%.1f GB; CPU only\n",
		roundDur(e.Total), len(p.Frames), roundDur(e.PerFrame), src, e.Processes, float64(e.PeakBytes)/1e9)
}

// confirmDevelop asks before hours of CPU. Without a terminal there is nobody to ask,
// so it takes --yes (unlike judge's spend check): a script or agent must say so.
func confirmDevelop(cmd *cobra.Command, e develop.Estimate, n int, yes bool) error {
	if yes {
		return nil
	}
	if !isTerminal(os.Stdin) {
		return fmt.Errorf("developing %d frame(s) takes ~%s of CPU: pass --yes to start without a terminal to confirm on", n, roundDur(e.Total))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Develop %d frame(s), ~%s? [y/N] ", n, roundDur(e.Total))
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("not confirmed: nothing was developed (pass --yes to skip this question)")
}

func describeRun(w io.Writer, out string, s develop.Summary) {
	var bytes int64
	for _, e := range s.Exports {
		bytes += e.Bytes
	}
	fmt.Fprintf(w, "developed %d, failed %d, up to date %d: %d JPEG(s), %.1f MB, in %s, in %s\n",
		s.Developed, len(s.Failures), s.UpToDate, len(s.Exports), float64(bytes)/1e6, out, roundDur(s.Elapsed))
	for _, l := range s.Logs {
		fmt.Fprintf(w, "LightCraft's logs for a failed chunk: %s\n", l)
	}
}

// failures lists the frames not developed, as the command's error.
func failures(w io.Writer, s develop.Summary) error {
	if len(s.Failures) == 0 {
		return nil
	}
	for _, f := range s.Failures {
		fmt.Fprintf(w, "  not developed: %s: %s\n", f.Name, f.Err)
	}
	return fmt.Errorf("%d frame(s) not developed: fix them and run cull develop again (it retries only those)", len(s.Failures))
}

// roundDur is d for people: seconds under ten minutes, then minutes.
func roundDur(d time.Duration) time.Duration {
	if d < 10*time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
