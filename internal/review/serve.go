package review

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/xmp"
)

// ServeOptions configure a review server.
type ServeOptions struct {
	ReportPath   string
	LabelsPath   string
	WriteXMP     bool // rewrite each changed frame's sidecar
	OverwriteXMP bool // also sidecars not written by cull
	Token        string
}

// Server saves the user's labels (and optionally sidecars) for one sheet.
type Server struct {
	sheet *Sheet
	o     ServeOptions
	files map[string]string // base name → the frame's path in the report
	mu    sync.Mutex        // one change at a time: log, sidecar, report
}

// NewServer refuses a report where two frames share a base name: a label couldn't
// say which one (or which sidecar) it means.
func NewServer(s *Sheet, rep *report.Report, o ServeOptions) (*Server, error) {
	if len(o.Token) < 32 {
		return nil, errors.New("review server: token too short")
	}
	if dups := labels.Duplicates(rep.Results); len(dups) > 0 {
		return nil, fmt.Errorf("frames share a file name, so labels can't tell them apart: %s", strings.Join(dups, "; "))
	}
	files := map[string]string{}
	for _, r := range rep.Results {
		files[filepath.Base(r.File)] = r.File
	}
	return &Server{sheet: s, o: o, files: files}, nil
}

// Handler serves the sheet for a listener at addr (host:port): the page, its
// images, and the labels API. Every request must name this listener as its Host
// (defeats DNS rebinding); an Origin, when sent, must be this listener; /api
// needs the session token.
func (s *Server) Handler(addr string) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	hosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /index.html", s.page)
	mux.HandleFunc("GET /api/labels", s.getLabels)
	mux.HandleFunc("POST /api/labels", s.postLabel)
	mux.HandleFunc("GET /"+AssetsDir+"/{name}", s.image)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !(strings.HasPrefix(o, "http://") && hosts[strings.TrimPrefix(o, "http://")]) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Cull-Token")), []byte(s.o.Token)) != 1 {
			http.Error(w, "bad or missing token: open the URL the server printed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	b, err := s.sheet.page(true, s.o.LabelsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// image serves the sheet's own JPEGs from its assets folder and nothing else.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !strings.HasSuffix(name, ".jpg") || name != filepath.Base(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.sheet.Dir, AssetsDir, name))
}

type state struct {
	Label string `json:"label"`
	Stars int    `json:"stars"`
}

func (s *Server) getLabels(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	m, err := labels.Read(s.o.LabelsPath)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]state{}
	for f, e := range m {
		out[f] = state{e.Label, e.Stars}
	}
	writeJSON(w, map[string]any{"labels": out})
}

func (s *Server) postLabel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		File  string `json:"file"`
		Label string `json:"label"`
		Stars int    `json:"stars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	path, ok := s.files[in.File]
	if !ok {
		http.Error(w, "not in the report: "+in.File, http.StatusBadRequest)
		return
	}
	e := labels.Entry{File: in.File, Label: in.Label, Stars: in.Stars, At: time.Now()}
	if err := e.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := labels.Append(s.o.LabelsPath, e); err != nil {
		http.Error(w, "not saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sidecar := "off"
	if s.o.WriteXMP {
		sidecar = s.writeSidecar(path, e)
	}
	writeJSON(w, map[string]any{"entry": e, "sidecar": sidecar})
}

// writeSidecar rewrites one frame's sidecar from its effective verdict and stars.
// A newly created sidecar is recorded in the report, re-read just before so that a
// concurrent writer loses at most this ownership record (which fails safe: the
// sidecar is then treated as foreign and never overwritten).
func (s *Server) writeSidecar(file string, e labels.Entry) string {
	rep, err := report.Load(s.o.ReportPath)
	if err != nil {
		return "skipped: " + err.Error()
	}
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.File != file {
			continue
		}
		before := r.XMP
		switch err := labels.WriteSidecar(r, e, false, s.o.OverwriteXMP); {
		case errors.Is(err, xmp.ErrExists):
			return "skipped: sidecar exists and wasn't written by cull"
		case err != nil:
			return "skipped: " + err.Error()
		}
		if r.XMP != before {
			if err := rep.Save(s.o.ReportPath); err != nil {
				return "written; report not updated: " + err.Error()
			}
		}
		return "written"
	}
	return "skipped: frame no longer in the report"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
