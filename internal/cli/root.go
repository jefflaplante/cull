// Package cli defines the cobra command tree.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/ui"
)

// Set via -ldflags "-X github.com/jefflaplante/cull/internal/cli.version=..."
var version = "dev"

// sharedOpts are persistent flags that apply to every subcommand that reads a shoot.
type sharedOpts struct {
	report            string
	recursive         bool
	maxEdge           int
	tiles             int
	minPreviewEdge    int
	faceMinQ          float64
	saveInputs        string
	landedWithSubject bool
	noReviewImages    bool
	seqGap            time.Duration
	seqLook           float64
	out               outputOpts
	tags              tagFlags // offload, scan, judge
}

func NewRootCmd() *cobra.Command {
	var so sharedOpts
	root := &cobra.Command{
		Use:   "cull",
		Short: "Cull DNG raw files with a vision model: sharpness gates, exposure is fixed, composition is cropped",
		Long: `cull extracts the embedded JPEG preview from each DNG, measures it, and asks a
vision model to assess sharpness, exposure, and composition. A deterministic policy
turns those assessments into keep / review / cull decisions.

A shoot, start to finish:
  cull offload <card> <dest> --name "…"   copy and verify the card (then a free scan)
  cull judge <dir>                         the model; re-run it to continue
  cull review <dir>                        you label and rate
  cull decide --sort <dir>                 before import
  cull apply-c1 --run <dir>                after import
cull status <dir> says where a shoot stands and what to run next.

Your own defaults for any flag go in ~/.cull ($CULL_CONFIG to use another file), one
"name = value" per line, with optional [command] sections:

  keep-best = 3
  outranked = cull
  [review]
  sort = all

A flag you type wins, then a shoot's stored policy and backend (decide, judge), then
~/.cull, then the built-in default. Each run prints the values it took from the file;
CULL_CONFIG=/dev/null turns it off for one run. One-off and risky flags can't be set
there (e.g. yes, fresh, force, rerank, estimate, run, overwrite-xmp, -o, -q/-v): a line
naming one is skipped with a warning.`,
		SilenceUsage:  true, // runtime errors shouldn't dump usage
		SilenceErrors: true, // main prints the error once
		Version:       version,
	}
	// Only the report path and recursion apply to every command; the image and
	// grouping flags go on the commands that use them, so no command accepts a
	// flag it would silently ignore. so starts at the defaults for commands
	// without them (base reads every field).
	so = sharedOpts{maxEdge: defaultMaxEdge, tiles: 1, minPreviewEdge: 1500, faceMinQ: 80, seqGap: defaultSeqGap, seqLook: group.DefaultLook}
	pf := root.PersistentFlags()
	pf.StringVarP(&so.report, "report", "o", "", "report path (default <dir>/cull-report.json)")
	pf.BoolVarP(&so.recursive, "recursive", "r", false, "recurse into subdirectories")
	so.out.register(pf)
	installHelp(root)
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		l, err := so.out.level()
		if err != nil {
			return err
		}
		return applyDotfile(cmd, cmd.ErrOrStderr(), l == ui.Quiet)
	}

	scan, judge, rank, decide := newScanCmd(&so), newCullCmd(&so), newRankCmd(&so), newDecideCmd(&so)
	for _, c := range []*cobra.Command{scan, judge} {
		so.registerPrep(c.Flags())
		so.tags.register(c.Flags())
		for _, n := range []string{"project", "event", "location", "keyword"} {
			c.Flags().MarkDeprecated(n, "set the shoot's tags with cull offload or cull tag <dir>")
		}
	}
	setSection(scan.Flags(), secCommon, "save-inputs")
	for _, c := range []*cobra.Command{scan, judge, rank, decide} {
		so.registerSeq(c.Flags())
	}
	root.AddCommand(newOffloadCmd(&so), newTagCmd(&so), scan, judge, rank, decide, newReviewCmd(&so), newCalibrateCmd(), newApplyC1Cmd(&so), newRestoreCmd(&so), newStatusCmd(&so), newImportLabelsCmd(&so), newVersionCmd())
	return root
}

const (
	defaultMaxEdge = 1024 // 2026-10-01 A/B: 16% fewer input tokens than 1568, verdicts within noise
	defaultSeqGap  = 60 * time.Second
)

// registerPrep adds the flags that shape what the model is sent (scan, judge).
func (so *sharedOpts) registerPrep(f *pflag.FlagSet) {
	f.IntVar(&so.maxEdge, "max-edge", defaultMaxEdge, "long edge of the full-frame image sent to the model (context only: sharpness is judged on the native-resolution crops)")
	f.IntVar(&so.tiles, "tiles", 1, "\"where focus landed\" tiles per image (native resolution)")
	f.IntVar(&so.minPreviewEdge, "min-preview-edge", 1500, "try exiftool / flag images whose embedded preview is smaller than this")
	f.Float64Var(&so.faceMinQ, "face-min-q", 80, "face detection score needed to trust a face as the focus target")
	f.StringVar(&so.saveInputs, "save-inputs", "", "write exactly what the model sees (JPEGs + inputs.json) to this directory")
	f.BoolVar(&so.landedWithSubject, "landed-with-subject", false, "also send \"where focus landed\" tiles when a subject crop exists (can bias the model toward texture)")
	f.BoolVar(&so.noReviewImages, "no-review-images", false, "don't write the review sheet's images (cull-review/assets) while previews are decoded; review renders them later")
	setSection(f, secTuning, "max-edge", "tiles", "min-preview-edge", "face-min-q", "save-inputs", "no-review-images")
	setSection(f, secExperimental, "landed-with-subject")
}

// registerSeq adds the grouping flags (the commands that group frames into sets).
func (so *sharedOpts) registerSeq(f *pflag.FlagSet) {
	registerSeqVars(f, &so.seqGap, &so.seqLook)
}

func registerSeqVars(f *pflag.FlagSet, gap *time.Duration, look *float64) {
	f.DurationVar(gap, "seq-gap", defaultSeqGap, "frames this close in capture time can link into a sequence (0 = no sequence grouping)")
	f.Float64Var(look, "seq-look", group.DefaultLook, "max look distance (0-1) to the previous frame for it to link into the same sequence")
	setSection(f, secPolicy, "seq-gap", "seq-look")
}

// base builds the pipeline config common to all subcommands from a dir argument.
func (so *sharedOpts) base(arg string) (pipeline.Config, error) {
	dir, err := filepath.Abs(arg)
	if err != nil {
		return pipeline.Config{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return pipeline.Config{}, fmt.Errorf("%s is not a directory", arg)
	}
	if err := validSeq(group.Options{Gap: so.seqGap, MaxLook: so.seqLook}); err != nil {
		return pipeline.Config{}, err
	}
	saveInputs := ""
	if so.saveInputs != "" {
		if saveInputs, err = filepath.Abs(so.saveInputs); err != nil {
			return pipeline.Config{}, err
		}
		if saveInputs == dir {
			return pipeline.Config{}, fmt.Errorf("refusing to write inputs into the shoot directory %s; pick another --save-inputs", dir)
		}
	}
	tags, err := so.tags.tags()
	if err != nil {
		return pipeline.Config{}, err
	}
	report := so.report
	if report == "" {
		report = filepath.Join(dir, "cull-report.json")
	}
	reviewImages := filepath.Join(filepath.Dir(report), "cull-review") // where review looks by default
	if so.noReviewImages {
		reviewImages = ""
	}
	return pipeline.Config{
		Dir:               dir,
		Recursive:         so.recursive,
		ReportPath:        report,
		MinPreviewEdge:    so.minPreviewEdge,
		Prep:              imageprep.Options{MaxEdge: so.maxEdge},
		LandedTiles:       so.tiles,
		FaceMinQ:          so.faceMinQ,
		Seq:               group.Options{Gap: so.seqGap, MaxLook: so.seqLook},
		LandedWithSubject: so.landedWithSubject,
		SaveInputs:        saveInputs,
		ReviewImages:      reviewImages,
		Concurrency:       4,
		CheckpointN:       25,
		Log:               os.Stderr,
		// Commands without policy flags (scan, offload's scan) still flag junk, as
		// judge would by default; judge and decide replace the policy from flags.
		Policy: eval.Policy{Junk: eval.ActionCull},
		Tags:   tags,
	}, nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
	}
}
