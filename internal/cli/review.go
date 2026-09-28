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
		out                  string
		jobs, port           int
		force, serve, openIt bool
		writeXMP, overwrite  bool
	)
	cmd := &cobra.Command{
		Use:   "review <dir>",
		Short: "HTML contact sheet for checking decisions, labeling and rating frames",
		Long: `review writes an HTML page (no network) from the report: every frame with the
subject crop the model judged, the decision and its reasons. Label frames
keep/review/cull (K/R/C, U clears) and rate them 1-5 stars (0 clears).

With --serve the sheet is served on 127.0.0.1 and every change is saved at once to
gophotocull-labels.jsonl beside the report; --write-xmp also rewrites that frame's
sidecar (your stars, verdict colour and keyword), which Capture One reads on
import. Without --serve, labels stay in the browser; export them from the page.
'calibrate', 'decide --labels' and 'apply-c1 --labels' read the log. Works on scan
reports too (labeling only).`,
		Example: `  gophotocull review --serve --open ~/Pictures/2026-09-26
  gophotocull review --serve --write-xmp ~/Pictures/2026-09-26`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			for _, name := range []string{"port", "open", "write-xmp", "overwrite-xmp"} {
				if fl.Changed(name) && !serve {
					return fmt.Errorf("--%s requires --serve", name)
				}
			}
			if overwrite && !writeXMP {
				return fmt.Errorf("--overwrite-xmp requires --write-xmp")
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
			if !serve {
				fmt.Fprintf(cmd.ErrOrStderr(), "review sheet: %s\nopen it with: open %q\n", sheet.Index, sheet.Index)
				return nil
			}
			return serveSheet(cmd, sheet, rep, review.ServeOptions{
				ReportPath: cfg.ReportPath, LabelsPath: labels.DefaultPath(cfg.ReportPath),
				WriteXMP: writeXMP, OverwriteXMP: overwrite,
			}, port, fl.Changed("port"), openIt)
		},
	}
	f := cmd.Flags()
	f.StringVar(&out, "out", "", "output directory (default: gophotocull-review next to the report)")
	f.IntVarP(&jobs, "concurrency", "j", 4, "parallel image rendering (~200 MB RAM each)")
	f.BoolVar(&force, "force", false, "re-render images that already exist")
	f.BoolVar(&serve, "serve", false, "serve the sheet on 127.0.0.1 and save every label to gophotocull-labels.jsonl beside the report")
	f.IntVar(&port, "port", 0, "port for --serve (default: fixed per report, so a restarted server keeps the page's origin and its queued changes; 0 = any free port)")
	f.BoolVar(&openIt, "open", false, "open the served sheet in the default browser")
	f.BoolVar(&writeXMP, "write-xmp", false, "with --serve: rewrite each changed frame's sidecar (your stars, verdict colour and keyword)")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "with --write-xmp: also overwrite sidecars gophotocull did not write")
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
	if o.WriteXMP {
		fmt.Fprintln(w, "sidecars: written on every change (never over ones gophotocull didn't write)")
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
