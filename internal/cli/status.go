package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
)

// labelSampleTarget is how many labelled frames make calibrate worth reading.
const labelSampleTarget = 30

func newStatusCmd(so *sharedOpts) *cobra.Command {
	var labelsPath string
	cmd := &cobra.Command{
		Use:   "status <dir>",
		Short: "Where a shoot stands (judged, labelled, ranked, spent) and what to run next",
		Long: `status reads the folder, its report and your labels log, and prints counts and
the next command to run. It changes nothing and calls no model.`,
		Example: "  cull status ~/Pictures/2026-09-26",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			files, err := pipeline.Discover(cfg.Dir, cfg.Recursive)
			if err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if errors.Is(err, fs.ErrNotExist) {
				rep = nil
			} else if err != nil {
				return err
			}
			var lab map[string]labels.Entry
			if rep != nil {
				rep.Relocate(cfg.ReportPath, cfg.Dir)
				if lab, err = labels.Read(labelsOr(labelsPath, cfg.ReportPath)); err != nil {
					return err
				}
			}
			writeStatus(cmd.OutOrStdout(), cfg, files, rep, lab)
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "your labels log (default: cull-labels.jsonl beside the report)")
	return cmd
}

func writeStatus(w io.Writer, cfg pipeline.Config, files []string, rep *report.Report, lab map[string]labels.Entry) {
	dir := shellQuote(cfg.Dir)
	if cfg.ReportPath != filepath.Join(cfg.Dir, "cull-report.json") {
		dir = "-o " + shellQuote(cfg.ReportPath) + " " + dir // suggested commands keep the same report
	}
	which, finish, unfinished := journal.Incomplete(cfg.Dir)
	// Hidden redate temps: one whose frame is missing may be that frame's only copy.
	// redate restores it by itself when its journal records the frame (it proves the
	// temp first); otherwise the user puts it back, or deletes it if it isn't the frame.
	// One beside its frame is settled by the next redate (removed, or kept and said why).
	jr, _ := journal.LoadRedate(cfg.Dir)
	orphanNext, missing := "", []string(nil)
	for _, d := range tempFolders(cfg.Dir, cfg.Recursive) {
		for tmp, target := range offload.RedateTemps(d) {
			rel, _ := filepath.Rel(cfg.Dir, target)
			if _, err := os.Lstat(target); err == nil {
				missing = append(missing, fmt.Sprintf("  hidden temp %s beside %s: the next redate removes it if %s is what it recorded, else keeps both and says why\n", tmp, rel, rel))
				continue
			}
			if rec, ok := jr.FileRecord(rel); ok && rec.Want != "" && unfinished && which == "redate" {
				missing = append(missing, fmt.Sprintf("  missing: %s; the next redate restores it from its hidden temp %s (proving it first)\n", rel, tmp))
				continue
			}
			if orphanNext == "" {
				orphanNext = "mv " + shellQuote(tmp) + " " + shellQuote(target) + "   (if it is the frame; if it isn't, delete it)"
			}
			missing = append(missing, fmt.Sprintf("  missing: %s; the hidden temp %s may be its only copy\n", rel, tmp))
		}
	}
	pending := func() {
		for _, m := range missing {
			fmt.Fprint(w, m)
		}
		if !unfinished {
			return
		}
		fmt.Fprintf(w, "  unfinished: a %s (cull-%s.json); judge, decide, review and restore refuse until it's finished\n", which, which)
	}
	if rep == nil {
		fmt.Fprintf(w, "%s: %d DNGs; no report at %s\n", cfg.Dir, len(files), cfg.ReportPath)
		pending()
		if orphanNext != "" || unfinished {
			fmt.Fprintf(w, "next: %s\n", cmp.Or(orphanNext, finish))
			return
		}
		fmt.Fprintf(w, "next: cull scan %s   (free), or cull judge --estimate %s\n", dir, dir)
		return
	}
	model := rep.Backend + "/" + rep.Model
	if rep.ResolvedModel != "" && rep.ResolvedModel != rep.Model {
		model += " → " + rep.ResolvedModel
	}
	if rep.Backend == "" {
		model = "scan only"
	}
	effort := rep.Effort
	if effort == "" {
		effort = "default"
	}
	// Frames moved into keep/ review/ cull/ (--sort; cull/ alone for --sort=culls, or
	// the old culled/ folder) are the shoot's too, though the folder walk skips them.
	var inCull, inKeepReview int
	for _, r := range rep.Results {
		if r.MovedTo == "" || !exists(r.MovedTo) {
			continue
		}
		switch filepath.Base(filepath.Dir(r.MovedTo)) {
		case pipeline.CullDir, pipeline.CulledDir:
			inCull++
		default:
			inKeepReview++
		}
	}
	where := ""
	if n := inCull + inKeepReview; n > 0 {
		where = fmt.Sprintf(" (%d sorted into folders)", n)
	}
	fmt.Fprintf(w, "%s: %d DNGs%s; report %s (%s, effort %s)\n", cfg.Dir, len(files)+inCull+inKeepReview, where, filepath.Base(cfg.ReportPath), model, effort)

	inReport := map[string]bool{}
	var assessed, junk, errs, moved int
	counts := map[string]int{}
	var labelled, rated, disagree int
	for _, r := range rep.Results {
		inReport[r.File] = true
		switch {
		case r.Error != "":
			errs++
		case r.Evaluation != nil:
			assessed++
			counts[string(r.Decision)]++
		case r.Junk != nil && r.Decision != "": // decided by --junk, without the model
			junk++
			counts[string(r.Decision)]++
		}
		if r.MovedTo != "" {
			moved++
		}
		if e, ok := lab[filepath.Base(r.File)]; ok {
			if e.Label != "" {
				labelled++
				if r.Decision != "" && string(r.Decision) != e.Label {
					disagree++
				}
			}
			if e.Stars > 0 {
				rated++
			}
		}
	}
	unjudged := 0
	for _, f := range files {
		if !inReport[f] {
			unjudged++
		}
	}
	fmt.Fprintf(w, "  assessed %d · junk %d · errors %d · not yet judged %d\n", assessed, junk, errs, unjudged)
	if assessed+junk > 0 {
		fmt.Fprintf(w, "  model: keep %d · review %d · cull %d · moved out of the shoot folder (keep/ review/ cull/) %d\n", counts["keep"], counts["review"], counts["cull"], moved)
	}
	fmt.Fprintf(w, "  you: labelled %d/%d · rated %d · disagree with the model %d\n", labelled, len(rep.Results), rated, disagree)
	byModel, byScores := 0, 0
	for _, s := range rep.Sets {
		if s.Of < 2 {
			continue
		}
		if s.By == "model" {
			byModel++
		} else {
			byScores++
		}
	}
	fmt.Fprintf(w, "  sets: %d (ranked %d, by scores %d)\n", byModel+byScores, byModel, byScores)
	if _, priced := llm.PriceFor(rep.Backend, rep.Model); priced {
		fmt.Fprintf(w, "  spent: $%.2f at list price\n", rep.Cost())
	} else if rep.Backend != "" {
		fmt.Fprintf(w, "  spent: not billed per token (%s)\n", rep.Backend)
	}
	judgeBatch := exists(cfg.ReportPath + ".batch.json")
	rankBatch := exists(cfg.ReportPath + ".rank-batch.json")
	switch {
	case judgeBatch:
		fmt.Fprintf(w, "  pending: a judge batch (%s.batch.json)\n", filepath.Base(cfg.ReportPath))
	case rankBatch:
		fmt.Fprintf(w, "  pending: a ranking batch (%s.rank-batch.json)\n", filepath.Base(cfg.ReportPath))
	}

	pending()

	var next string
	switch {
	case orphanNext != "":
		next = orphanNext
	case unfinished:
		next = finish
	case judgeBatch:
		next = "cull judge --batch " + dir + "   (re-attaches; already paid for)"
	case rankBatch:
		next = "cull judge --batch " + dir + "   (re-attaches; already paid for)"
	case rep.Backend == "":
		next = "cull judge --estimate " + dir + "   (then cull judge)"
	case unjudged > 0:
		next = "cull judge " + dir
	case byScores > 0:
		next = fmt.Sprintf("cull judge --estimate %s   (then cull judge: it ranks the %d unranked set(s))", dir, byScores)
	case labelled < min(labelSampleTarget, len(rep.Results)):
		next = fmt.Sprintf("label a sample in cull review %s (%d/%d so far), then cull calibrate %s", dir, labelled, min(labelSampleTarget, len(rep.Results)), dir)
	default:
		// The same placement the shoot already uses: --sort=culls for a shoot whose
		// only moved frames are culls; otherwise --sort, the usual workflow.
		move := "--sort"
		if inCull > 0 && inKeepReview == 0 {
			move = "--sort=culls"
		}
		next = "cull calibrate " + dir + ", then cull decide " + move + " " + dir
	}
	fmt.Fprintf(w, "next: %s\n", next)
}

// shellQuote makes a path safe to paste into a POSIX shell: single quotes unless it
// needs none.
func shellQuote(s string) string { return journal.ShellQuote(s) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// tempFolders are the folders redate may leave temps in: dir, its sort folders, and
// with recursive every folder below (hidden ones skipped).
func tempFolders(dir string, recursive bool) []string {
	if !recursive {
		out := []string{dir}
		for _, d := range offload.MovedDirs {
			out = append(out, filepath.Join(dir, d))
		}
		return out
	}
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		out = append(out, p)
		return nil
	})
	return out
}
