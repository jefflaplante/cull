package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/review"
)

func newReviewCmd(so *sharedOpts) *cobra.Command {
	var (
		out                      string
		jobs, port               int
		force, static            bool
		prepare, clearCache      bool
		sortAfter                sortMode
		noOpen, noXMP, overwrite bool
		labelsPath               string
	)
	cmd := &cobra.Command{
		Use:   "review <dir>",
		Short: "Review, label and rate frames in your browser; every change is saved",
		Long: `review builds an HTML contact sheet from the report (every frame with the subject
crop the model judged, the decision and its reasons), serves it on 127.0.0.1 and
opens it in your browser. Label frames keep/review/cull (K/R/C, U clears) and rate
them 1-5 stars (0 clears). Every change is saved at once to cull-labels.jsonl
beside the report, and the frame's .xmp sidecar is rewritten (your stars, verdict
colour and keyword), which Capture One reads on import; sidecars cull didn't write
are never touched. Ctrl-C stops the server.

--no-xmp saves only the labels log; --no-open doesn't launch the browser.
'calibrate', 'decide' and 'apply-c1' read the log.
Works on scan reports too (labeling only).

The sheet's images (thumbnails, subject crops, 100% loupe views) are a cache in
cull-review/assets. scan and judge fill it while each preview is decoded anyway, so a
judged shoot opens without rendering. Each image is named after the file and focus box
it was made from: review renders only what's missing or changed and removes stale
images. --prepare fills the cache and exits; --clear-cache empties it; --force
renders everything again.

Files never move while the page is open: a label change rewrites only the labels log
and the frame's sidecar, where the frame is now. --sort re-sorts once, when you stop the
server with Ctrl-C, into keep/, review/ and cull/ by your labels (--sort=culls: only culls into
cull/; as decide --sort does, with the report's stored policy). If the server ends any other way, nothing moves: run
cull decide --sort yourself. Don't re-sort after keep/ and review/ are imported into
Capture One or Lightroom: they lose track of files that move; use apply-c1 instead.`,
		Example: `  cull review ~/Pictures/2026-09-26
  cull review --no-xmp ~/Pictures/2026-09-26     # labels only, no sidecars
  cull review --prepare ~/Pictures/2026-09-26    # fill the image cache now, open later
  cull review --clear-cache ~/Pictures/2026-09-26
  cull review --sort ~/Pictures/2026-09-26       # re-sort by your labels when you stop`,
		Args: sortArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			for mode, on := range map[string]bool{"static": static, "prepare": prepare} {
				if !on {
					continue
				}
				for _, name := range []string{"port", "no-open", "no-xmp", "overwrite-xmp", "sort"} {
					if fl.Changed(name) {
						return fmt.Errorf("--%s can't be used with --%s: it only applies when serving", name, mode)
					}
				}
			}
			if overwrite && noXMP {
				return fmt.Errorf("--overwrite-xmp can't be used with --no-xmp")
			}
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			if err := refuseUnfinished(cfg.Dir, false); err != nil {
				return err
			}
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return fmt.Errorf("no report: %w (run scan or judge first, or pass -o)", err)
			}
			if sortAfter != sortNone && rep.Backend == "" {
				return fmt.Errorf("--sort sorts by verdict, and %s is a scan report with no judged frames: run cull judge first", cfg.ReportPath)
			}
			if rep.Relocate(cfg.ReportPath, cfg.Dir) { // the folder was renamed: the server reloads the report, so save the new paths
				if err := rep.Save(cfg.ReportPath); err != nil {
					return err
				}
			}
			if out == "" {
				out = filepath.Join(filepath.Dir(cfg.ReportPath), "cull-review")
			}
			if clearCache {
				n, err := review.ClearCache(out)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "removed %d cached image(s) from %s\n", n, filepath.Join(out, review.AssetsDir))
				if !prepare {
					return nil
				}
			}
			ro := so.out.newOutput(cmd, true) // decoding every preview takes a while on a big shoot
			sheet, err := review.Build(rep, cfg.ReportPath, review.Options{Out: out, Concurrency: jobs, Force: force, UI: ro.UI}, ro.Log)
			ro.Close()
			if err != nil {
				return err
			}
			if prepare {
				fmt.Fprintf(cmd.ErrOrStderr(), "review images ready in %s; open the sheet with: cull review %q\n", filepath.Join(out, review.AssetsDir), args[0])
				return nil
			}
			if static {
				fmt.Fprintf(cmd.ErrOrStderr(), "review sheet: %s\nopen it with: open %q\n", sheet.Index, sheet.Index)
				return nil
			}
			if err := serveSheet(cmd, sheet, rep, review.ServeOptions{
				ReportPath: cfg.ReportPath, LabelsPath: labelsOr(labelsPath, cfg.ReportPath),
				WriteXMP: !noXMP, OverwriteXMP: overwrite,
			}, port, fl.Changed("port"), !noOpen, sortAfter); err != nil || sortAfter == sortNone {
				return err
			}
			return sortAfterReview(cmd, so, cfg, labelsPath, sortAfter)
		},
	}
	f := cmd.Flags()
	f.StringVar(&out, "out", "", "output directory for index.html, with the images in its assets/ folder (default: cull-review next to the report)")
	f.IntVarP(&jobs, "concurrency", "j", min(runtime.NumCPU(), 8), "parallel image rendering (~200 MB RAM each)")
	f.BoolVar(&force, "force", false, "render every image again, even current ones")
	f.BoolVar(&prepare, "prepare", false, "render missing or changed images into the cache, then exit (no server)")
	registerSort(f, &sortAfter, "when the server stops (Ctrl-C), re-sort the frames by your labels, as decide --sort does (--sort=culls: only culls into cull/); don't use it once the folders are imported")
	f.BoolVar(&clearCache, "clear-cache", false, "delete the sheet's cached images and exit (with --prepare: then render them again)")
	f.BoolVar(&static, "static", false, "write an offline index.html instead of serving (labels stay in the browser)")
	f.MarkDeprecated("static", "the offline page goes in the next release: use cull review (served), which saves every change itself")
	f.BoolVar(&noOpen, "no-open", false, "don't open the browser; open the printed URL yourself")
	f.BoolVar(&noXMP, "no-xmp", false, "don't write sidecars; save only the labels log")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "also overwrite sidecars not written by cull")
	f.StringVar(&labelsPath, "labels", "", "the labels log to save to (default: cull-labels.jsonl beside the report)")
	f.IntVar(&port, "port", 0, "port (default: fixed per report, so a restarted server keeps the page's origin and its queued changes; 0 = any free port)")
	setSection(f, secSidecars, "no-xmp", "overwrite-xmp", "labels")
	setSection(f, secTuning, "out", "concurrency", "force", "port")
	return cmd
}

// serveSheet runs the review server on 127.0.0.1 until the command's context ends
// (Ctrl-C).
func serveSheet(cmd *cobra.Command, sheet *review.Sheet, rep *report.Report, o review.ServeOptions, port int, explicit, openIt bool, sortAfter sortMode) error {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	o.Token = hex.EncodeToString(tok)
	srv, err := review.NewServer(sheet, rep, o)
	if err != nil {
		return err
	}
	w := cmd.ErrOrStderr()
	if !explicit {
		port = defaultPort(o.ReportPath)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil && !explicit {
		fmt.Fprintf(w, "port %d is busy (another review of this report?): using a random port; changes a page queued while its server was down won't carry over\n", port)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	url := "http://" + addr + "/#token=" + o.Token
	switch sortAfter {
	case sortAll:
		fmt.Fprintln(w, "--sort: files stay where they are while you work; when you stop, frames are re-sorted into keep/, review/ and cull/ by your labels")
	case sortCulls:
		fmt.Fprintln(w, "--sort=culls: files stay where they are while you work; when you stop, culls are moved into cull/ by your labels")
	}
	fmt.Fprintf(w, "review server: %s\nlabels: %s\n", url, o.LabelsPath)
	switch {
	case o.WriteXMP && o.OverwriteXMP:
		fmt.Fprintln(w, "sidecars: written on every change, including over ones not written by cull (--overwrite-xmp)")
	case o.WriteXMP:
		fmt.Fprintln(w, "sidecars: written on every change, never over ones not written by cull (--no-xmp to stop)")
	}
	fmt.Fprintln(w, "Ctrl-C to stop. Don't run judge on this report while reviewing.")
	if openIt {
		if err := exec.Command("open", url).Start(); err != nil {
			fmt.Fprintf(w, "could not open a browser (%v): open the URL above\n", err)
		}
	}
	hs := &http.Server{Handler: srv.Handler(addr), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-cmd.Context().Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// defaultPort is fixed per report (in the dynamic range), so a restarted server
// serves the page from the same origin and the browser's queued changes carry over.
func defaultPort(reportPath string) int {
	if abs, err := filepath.Abs(reportPath); err == nil {
		reportPath = abs
	}
	h := fnv.New32a()
	h.Write([]byte(reportPath))
	return 49152 + int(h.Sum32()%16384)
}

// labelsOr is path, or the default log beside the report.
func labelsOr(path, reportPath string) string {
	if path != "" {
		return path
	}
	return labels.DefaultPath(reportPath)
}

// sortAfterReview re-sorts the shoot once the review server has stopped: decide
// --sort with the report's stored policy (review has no policy flags, so verdicts
// stay as decided) and the labels just saved. It runs on its own context, since the
// command's was cancelled by the Ctrl-C that stopped the server.
func sortAfterReview(cmd *cobra.Command, so *sharedOpts, cfg pipeline.Config, labelsPath string, mode sortMode) error {
	w := cmd.ErrOrStderr()
	rep, err := report.Load(cfg.ReportPath)
	if err != nil {
		return err
	}
	var pol policyFlags
	fs := pflag.NewFlagSet("stored", pflag.ContinueOnError) // nothing typed: the stored policy, or the defaults
	pol.register(fs)
	p, _, err := pol.resolve(fs, rep.Policy)
	if err != nil {
		return fmt.Errorf("the report's stored policy: %w", err)
	}
	var gap time.Duration
	var look float64
	registerSeqVars(fs, &gap, &look)
	seq, _ := resolveSeq(fs, group.Options{Gap: gap, MaxLook: look}, rep.Seq)
	lab, err := userLabels(w, cfg.ReportPath, labelsPath, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "re-sorting by your labels (--sort)…")
	out := so.out.newOutput(cmd, true)
	moveCulls, sortEverything := mode.flags()
	sum, err := pipeline.Decide(context.Background(), cfg.ReportPath, pipeline.DecideOptions{
		Dir: cfg.Dir, Policy: p, MoveCulled: moveCulls, Sort: sortEverything, Seq: seq, Labels: lab, UI: out.UI,
	}, out.Log)
	out.Close()
	if err != nil {
		return err
	}
	fmt.Fprintln(w, describeDecide(sum, mode == sortAll))
	return nil
}
