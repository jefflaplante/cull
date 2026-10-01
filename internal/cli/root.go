// Package cli defines the cobra command tree.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/pipeline"
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
	seqGap            time.Duration
	seqLook           float64
}

func NewRootCmd() *cobra.Command {
	var so sharedOpts
	root := &cobra.Command{
		Use:   "cull",
		Short: "Cull DNG raw files with a vision model: sharpness gates, exposure is fixed, composition is cropped",
		Long: `cull extracts the embedded JPEG preview from each DNG, measures it, and asks a
vision model to assess sharpness, exposure, and composition. A deterministic policy
turns those assessments into keep / review / cull decisions.

Start with 'cull scan <dir>' to confirm preview resolution before spending tokens,
then 'cull judge <dir>' (the model), 'cull review <dir>' (you), 'cull decide'.`,
		SilenceUsage:  true, // runtime errors shouldn't dump usage
		SilenceErrors: true, // main prints the error once
		Version:       version,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&so.report, "report", "o", "", "report path (default <dir>/cull-report.json)")
	pf.BoolVarP(&so.recursive, "recursive", "r", false, "recurse into subdirectories")
	pf.IntVar(&so.maxEdge, "max-edge", 1024, "long edge of the full-frame image sent to the model (context only: sharpness is judged on the native-resolution crops)")
	pf.IntVar(&so.tiles, "tiles", 1, "\"where focus landed\" tiles per image (native resolution)")
	pf.IntVar(&so.minPreviewEdge, "min-preview-edge", 1500, "try exiftool / flag images whose embedded preview is smaller than this")
	pf.Float64Var(&so.faceMinQ, "face-min-q", 80, "face detection score needed to trust a face as the focus target")
	pf.StringVar(&so.saveInputs, "save-inputs", "", "write exactly what the model sees (JPEGs + inputs.json) to this directory")
	pf.BoolVar(&so.landedWithSubject, "landed-with-subject", false, "also send \"where focus landed\" tiles when a subject crop exists (can bias the model toward texture)")
	pf.DurationVar(&so.seqGap, "seq-gap", 60*time.Second, "frames this close in capture time can link into a sequence (0 = no sequence grouping)")
	pf.Float64Var(&so.seqLook, "seq-look", group.DefaultLook, "max look distance (0-1) to the previous frame for it to link into the same sequence")

	root.AddCommand(newScanCmd(&so), newCullCmd(&so), newRankCmd(&so), newDecideCmd(&so), newReviewCmd(&so), newCalibrateCmd(), newApplyC1Cmd(&so), newRestoreCmd(&so), newVersionCmd())
	return root
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
	report := so.report
	if report == "" {
		report = filepath.Join(dir, "cull-report.json")
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
		Concurrency:       4,
		CheckpointN:       25,
		Log:               os.Stderr,
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
