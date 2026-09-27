// Package cli defines the cobra command tree.
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/imageprep"
	"github.com/jefflaplante/gophotocull/internal/pipeline"
)

// Set via -ldflags "-X github.com/jefflaplante/gophotocull/internal/cli.version=..."
var version = "dev"

// sharedOpts are persistent flags that apply to every subcommand that reads a shoot.
type sharedOpts struct {
	report         string
	recursive      bool
	maxEdge        int
	tiles          int
	minPreviewEdge int
	faceMinQ       float64
	saveInputs     string
}

func NewRootCmd() *cobra.Command {
	var so sharedOpts
	root := &cobra.Command{
		Use:   "gophotocull",
		Short: "Cull Leica DNGs with a vision model: sharpness gates, exposure is fixed, composition is cropped",
		Long: `gophotocull extracts the embedded JPEG preview from each DNG, measures it, and
asks a vision model to assess sharpness, exposure, and composition. A deterministic
policy turns those assessments into keep / review / cull decisions.

Start with 'gophotocull scan <dir>' to confirm preview resolution before spending tokens.`,
		SilenceUsage:  true, // runtime errors shouldn't dump usage
		SilenceErrors: true, // main prints the error once
		Version:       version,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&so.report, "report", "o", "", "report path (default <dir>/gophotocull-report.json)")
	pf.BoolVarP(&so.recursive, "recursive", "r", false, "recurse into subdirectories")
	pf.IntVar(&so.maxEdge, "max-edge", 1568, "long edge of the full-frame image sent to the model")
	pf.IntVar(&so.tiles, "tiles", 1, "\"where focus landed\" tiles per image (native resolution)")
	pf.IntVar(&so.minPreviewEdge, "min-preview-edge", 1500, "try exiftool / flag images whose embedded preview is smaller than this")
	pf.Float64Var(&so.faceMinQ, "face-min-q", 80, "face detection score needed to trust a face as the focus target")
	pf.StringVar(&so.saveInputs, "save-inputs", "", "write exactly what the model sees (JPEGs + inputs.json) to this directory")

	root.AddCommand(newScanCmd(&so), newCullCmd(&so), newRestoreCmd(&so), newVersionCmd())
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
		report = filepath.Join(dir, "gophotocull-report.json")
	}
	return pipeline.Config{
		Dir:            dir,
		Recursive:      so.recursive,
		ReportPath:     report,
		MinPreviewEdge: so.minPreviewEdge,
		Prep:           imageprep.Options{MaxEdge: so.maxEdge},
		LandedTiles:    so.tiles,
		FaceMinQ:       so.faceMinQ,
		SaveInputs:     saveInputs,
		Concurrency:    4,
		CheckpointN:    25,
		Log:            os.Stderr,
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
