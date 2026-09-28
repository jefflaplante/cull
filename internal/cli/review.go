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
	"time"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/review"
)

func newReviewCmd(so *sharedOpts) *cobra.Command {
	var (
		out                      string
		jobs, port               int
		force, static            bool
		noOpen, noXMP, overwrite bool
	)
	cmd := &cobra.Command{
		Use:   "review <dir>",
		Short: "Review, label and rate frames in your browser; every change is saved",
		Long: `review builds an HTML contact sheet from the report (every frame with the subject
crop the model judged, the decision and its reasons), serves it on 127.0.0.1 and
opens it in your browser. Label frames keep/review/cull (K/R/C, U clears) and rate
them 1-5 stars (0 clears). Every change is saved at once to gophotocull-labels.jsonl
beside the report, and the frame's .xmp sidecar is rewritten (your stars, verdict
colour and keyword), which Capture One reads on import; sidecars gophotocull didn't
write are never touched. Ctrl-C stops the server.

--no-xmp saves only the labels log; --no-open doesn't launch the browser; --static
writes an offline index.html instead of serving (labels then stay in the browser;
export them from the page). 'calibrate', 'decide' and 'apply-c1' read the log.
Works on scan reports too (labeling only).`,
		Example: `  gophotocull review ~/Pictures/2026-09-26
  gophotocull review --no-xmp ~/Pictures/2026-09-26     # labels only, no sidecars
  gophotocull review --static ~/Pictures/2026-09-26     # offline page`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			if static {
				for _, name := range []string{"port", "no-open", "no-xmp", "overwrite-xmp"} {
					if fl.Changed(name) {
						return fmt.Errorf("--%s can't be used with --static: it only applies when serving", name)
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
			rep, err := report.Load(cfg.ReportPath)
			if err != nil {
				return fmt.Errorf("no report: %w (run scan or cull first, or pass -o)", err)
			}
			if out == "" {
				out = filepath.Join(filepath.Dir(cfg.ReportPath), "gophotocull-review")
			}
			sheet, err := review.Build(rep, cfg.ReportPath, review.Options{Out: out, Concurrency: jobs, Force: force}, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if static {
				fmt.Fprintf(cmd.ErrOrStderr(), "review sheet: %s\nopen it with: open %q\n", sheet.Index, sheet.Index)
				return nil
			}
			return serveSheet(cmd, sheet, rep, review.ServeOptions{
				ReportPath: cfg.ReportPath, LabelsPath: labels.DefaultPath(cfg.ReportPath),
				WriteXMP: !noXMP, OverwriteXMP: overwrite,
			}, port, fl.Changed("port"), !noOpen)
		},
	}
	f := cmd.Flags()
	f.StringVar(&out, "out", "", "output directory for the sheet's images (default: gophotocull-review next to the report)")
	f.IntVarP(&jobs, "concurrency", "j", 4, "parallel image rendering (~200 MB RAM each)")
	f.BoolVar(&force, "force", false, "re-render images that already exist")
	f.BoolVar(&static, "static", false, "write an offline index.html instead of serving (labels stay in the browser)")
	f.BoolVar(&noOpen, "no-open", false, "don't open the browser; open the printed URL yourself")
	f.BoolVar(&noXMP, "no-xmp", false, "don't write sidecars; save only the labels log")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "also overwrite sidecars gophotocull did not write")
	f.IntVar(&port, "port", 0, "port (default: fixed per report, so a restarted server keeps the page's origin and its queued changes; 0 = any free port)")
	return cmd
}

// serveSheet runs the review server on 127.0.0.1 until the command's context ends
// (Ctrl-C).
func serveSheet(cmd *cobra.Command, sheet *review.Sheet, rep *report.Report, o review.ServeOptions, port int, explicit, openIt bool) error {
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
	fmt.Fprintf(w, "review server: %s\nlabels: %s\n", url, o.LabelsPath)
	switch {
	case o.WriteXMP && o.OverwriteXMP:
		fmt.Fprintln(w, "sidecars: written on every change, including over ones gophotocull didn't write (--overwrite-xmp)")
	case o.WriteXMP:
		fmt.Fprintln(w, "sidecars: written on every change, never over ones gophotocull didn't write (--no-xmp to stop)")
	}
	fmt.Fprintln(w, "Ctrl-C to stop. Don't run cull on this report while reviewing.")
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
