package develop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/cull/internal/offload"
)

const (
	// DefaultPerFrame and PeakPerProcess are LightCraft 0.4.0's cost on a 60 MP frame,
	// CPU only (2026-10-08): about 25 s per frame, and about 2.9 GB resident per
	// lightcraft-cli process, whose frames develop one at a time. The estimate uses the
	// shoot's own measured time once a run has recorded one.
	DefaultPerFrame = 25 * time.Second
	PeakPerProcess  = int64(2_900_000_000)

	// tempPrefix starts every export's temp name in the export folder: LightCraft writes
	// .cull-develop-<hex>.<stem>.jpg, which takes its name only once it checks out.
	// Never "._" (macOS's AppleDouble companions on exFAT/FAT).
	tempPrefix = ".cull-develop-"
)

// Options configure a develop run.
type Options struct {
	StatePath  string // cull-develop.json
	Work       string // the hidden work folder (.cull-develop)
	Out        string // the export folder
	LightCraft string // lightcraft-cli
	Recipe     Recipe
	Jobs       int  // LightCraft processes at once (each ~PeakPerProcess)
	Chunk      int  // frames per LightCraft process
	Force      bool // develop up-to-date frames again
	Log        io.Writer
}

// Plan is what a run will do, worked out without writing anything: --dry-run prints it.
type Plan struct {
	Frames     []Frame        // to develop, in keep-set order
	UpToDate   int            // keeps whose JPEG matches their fingerprint
	Unreadable []Failure      // keeps whose DNG or sidecar couldn't be read
	NoPreset   map[string]int // camera model → frames to develop without a preset (develop.auto instead)
	Estimate   Estimate
	state      *State
	fps        map[string]string // name → fingerprint
}

// Estimate is a run's expected cost: time and memory, CPU only.
type Estimate struct {
	PerFrame  time.Duration
	Total     time.Duration // wall clock at the run's parallelism
	Processes int           // LightCraft processes at once
	PeakBytes int64
	Measured  bool // PerFrame is this shoot's recorded mean, not DefaultPerFrame
}

// Summary is a run's outcome.
type Summary struct {
	Developed int
	UpToDate  int
	Exports   []Exported
	Failures  []Failure
	Logs      []string // work folders kept for their LightCraft logs (failed chunks)
	Elapsed   time.Duration
}

// Exported is one JPEG written.
type Exported struct {
	Name, Path string
	Bytes      int64
}

// Failure is a keep that wasn't developed, and why.
type Failure struct{ Name, Err string }

// jpegName is the export's name: the frame's stem, .jpg.
func jpegName(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) + ".jpg" }

// Prepare reads the keeps and the state, and plans the run: which keeps are up to date,
// which to develop, with what, and at what cost. It writes nothing.
func Prepare(o Options, keeps []Keep) (*Plan, error) {
	if err := o.Recipe.Validate(); err != nil {
		return nil, err
	}
	if o.Jobs < 1 || o.Chunk < 1 {
		return nil, fmt.Errorf("jobs %d, chunk %d: want at least 1 each", o.Jobs, o.Chunk)
	}
	stems := map[string]string{}
	for _, k := range keeps {
		j := strings.ToLower(jpegName(k.Name))
		if prev, ok := stems[j]; ok {
			return nil, fmt.Errorf("%s and %s would both export as %s", prev, k.Name, jpegName(k.Name))
		}
		stems[j] = k.Name
	}
	st, err := loadState(o.StatePath)
	if err != nil {
		return nil, err
	}
	p := &Plan{NoPreset: map[string]int{}, state: st, fps: map[string]string{}}
	for _, k := range keeps {
		f, err := Load(k)
		if err != nil {
			p.Unreadable = append(p.Unreadable, Failure{k.Name, err.Error()})
			continue
		}
		fp := o.Recipe.Fingerprint(f)
		p.fps[f.Name] = fp
		if !o.Force && upToDate(st.Frames[f.Name], fp, filepath.Join(o.Out, jpegName(f.Name))) {
			p.UpToDate++
			continue
		}
		if _, ok := o.Recipe.preset(f); o.Recipe.Presets && !ok {
			m := strings.TrimSpace(f.Model)
			if m == "" {
				m = "(no camera model)"
			}
			p.NoPreset[m]++
		}
		p.Frames = append(p.Frames, f)
	}
	p.Estimate = estimate(st, len(p.Frames), o.Jobs, o.Chunk)
	return p, nil
}

// upToDate: the frame's last develop succeeded from the same inputs, and its JPEG is
// still where it went, at its size.
func upToDate(rec *FrameState, fp, jpeg string) bool {
	if rec == nil || rec.Status != statusDone || rec.Fingerprint != fp || rec.JPEG != jpeg {
		return false
	}
	fi, err := os.Stat(jpeg)
	return err == nil && fi.Size() == rec.Bytes
}

func estimate(st *State, n, jobs, chunk int) Estimate {
	e := Estimate{PerFrame: DefaultPerFrame}
	var sum float64
	var k int
	for _, f := range st.Frames {
		if f.Status == statusDone && f.Ms > 0 {
			sum += f.Ms
			k++
		}
	}
	if k > 0 {
		e.PerFrame, e.Measured = time.Duration(sum/float64(k)*float64(time.Millisecond)), true
	}
	if n == 0 {
		return e
	}
	e.Processes = min(jobs, (n+chunk-1)/chunk)
	e.Total = time.Duration((n+e.Processes-1)/e.Processes) * e.PerFrame
	e.PeakBytes = int64(e.Processes) * PeakPerProcess
	return e
}

// Run develops the plan's frames: chunks of o.Chunk frames, each one headless
// lightcraft-cli process with its own throwaway library (so every attempt starts from
// the sidecars as they are now), o.Jobs at a time. Each export is written to a hidden
// temp in the export folder, checked, and given its name; the state is saved after
// every frame. Failed frames are reported in the summary, not as an error; the error
// is for the run itself (the lock, LightCraft not running, ctx cancelled).
func Run(ctx context.Context, o Options, p *Plan) (Summary, error) {
	start := time.Now()
	sum := Summary{UpToDate: p.UpToDate, Failures: append([]Failure(nil), p.Unreadable...)}
	if len(p.Frames) == 0 {
		return sum, nil
	}
	release, err := lockWork(o.Work)
	if err != nil {
		return sum, err
	}
	defer release()
	ver, err := version(ctx, o.LightCraft)
	if err != nil {
		return sum, err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return sum, err
	}
	if err := cleanWork(o.Work, o.Out); err != nil {
		return sum, err
	}
	presets := filepath.Join(o.Work, "presets")
	if err := os.MkdirAll(presets, 0o755); err != nil {
		return sum, err
	}
	for _, f := range p.Frames {
		if pr, ok := o.Recipe.preset(f); ok {
			if err := os.WriteFile(filepath.Join(presets, pr.File), pr.Data, 0o644); err != nil {
				return sum, err
			}
		}
	}
	st := p.state
	st.Out, st.Recipe, st.LightCraft = o.Out, o.Recipe, ver
	if err := st.save(o.StatePath); err != nil {
		return sum, err
	}
	r := &run{o: o, p: p, st: st, sum: &sum, presets: presets}
	var chunks [][]Frame
	for i := 0; i < len(p.Frames); i += o.Chunk {
		chunks = append(chunks, p.Frames[i:min(i+o.Chunk, len(p.Frames))])
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(o.Jobs, len(chunks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				r.chunk(ctx, i, chunks[i])
			}
		}()
	}
feed:
	for i := range chunks {
		select {
		case work <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	sum.Elapsed = time.Since(start)
	if r.saveErr != nil {
		return sum, r.saveErr
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	if len(sum.Logs) == 0 {
		release()
		os.RemoveAll(o.Work)
	}
	return sum, nil
}

// cleanWork removes what an interrupted run left: export temps (always cull's, by
// name) and chunk folders. The lock and the preset copies stay.
func cleanWork(work, out string) error {
	ents, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), tempPrefix) && !e.IsDir() {
			if err := os.Remove(filepath.Join(out, e.Name())); err != nil {
				return err
			}
		}
	}
	ents, err = os.ReadDir(work)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "chunk-") {
			if err := os.RemoveAll(filepath.Join(work, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// run is one Run's shared state; mu guards st, sum and the log.
type run struct {
	o       Options
	p       *Plan
	presets string
	mu      sync.Mutex
	st      *State
	sum     *Summary
	saveErr error
}

// frameRun is one frame within a chunk's develop script.
type frameRun struct {
	f          Frame
	id         int
	temp       string
	first, end int // its commands' indexes in the script: [first, end)
	errs       []string
	ms         float64
	settled    bool
}

func (r *run) chunk(ctx context.Context, idx int, frames []Frame) {
	dir := filepath.Join(r.o.Work, fmt.Sprintf("chunk-%04d", idx))
	keep := false // a frame failed: keep the chunk's folder for its LightCraft logs
	defer func() {
		if !keep {
			os.RemoveAll(dir)
			return
		}
		r.mu.Lock()
		r.sum.Logs = append(r.sum.Logs, dir)
		r.mu.Unlock()
	}()
	fail := func(f Frame, msg string) {
		keep = true
		r.record(f, nil, msg)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		for _, f := range frames {
			fail(f, err.Error())
		}
		return
	}
	lib := filepath.Join(dir, "library")

	// Import: LightCraft reads each frame's sidecar (exposure, crop) on import. Its
	// photo ids, which every later command names, come from the catalog.
	paths := make([]string, len(frames))
	for i, f := range frames {
		paths[i] = f.Path
	}
	importErr := map[string]string{} // path → why not
	ids := map[string]int{}          // file name → photo id
	var cmdErr string
	_, err := lightcraft(ctx, r.o.LightCraft, lib, dir, "import", []Command{
		{"library.xmpPreferences", map[string]any{"autoWrite": false}}, // never write beside the DNGs
		{"library.import", map[string]any{"paths": paths, "mode": "add"}},
		// The library is this chunk's alone, so it holds at most these frames: the limit is
		// exact, and a total above it (photos cull didn't import) is an error, not a truncation.
		{"catalog.query", map[string]any{"limit": len(frames)}},
	}, func(i int, l line) {
		if !l.OK {
			cmdErr = l.Command + ": " + l.errText()
			return
		}
		// An answer that doesn't parse means LightCraft's output format changed: say so,
		// rather than leave every frame "not imported".
		unexpected := func(what string, err error) {
			if cmdErr == "" {
				cmdErr = fmt.Sprintf("%s: couldn't read LightCraft's answer (%s): %v; is this lightcraft-cli a version cull knows (0.4.0)?", l.Command, what, err)
			}
		}
		switch l.Command {
		case "library.import":
			var res struct{ Failed [][2]string }
			if err := json.Unmarshal(l.Result, &res); err != nil {
				unexpected("failed: [[path, reason]]", err)
				return
			}
			for _, f := range res.Failed {
				importErr[f[0]] = f[1]
			}
		case "catalog.query":
			var res struct {
				Photos []struct {
					ID       int    `json:"id"`
					FileName string `json:"fileName"`
				}
				Total int `json:"total"`
			}
			if err := json.Unmarshal(l.Result, &res); err != nil {
				unexpected("photos: [{id, fileName}]", err)
				return
			}
			if res.Total > len(res.Photos) {
				unexpected("photos: [{id, fileName}]", fmt.Errorf("it lists %d of %d photos in a library of %d imports", len(res.Photos), res.Total, len(frames)))
				return
			}
			for _, ph := range res.Photos {
				if ph.ID == 0 || ph.FileName == "" {
					unexpected("photos: [{id, fileName}]", fmt.Errorf("a photo without an id or fileName"))
					return
				}
				ids[ph.FileName] = ph.ID
			}
		}
	})
	if ctx.Err() != nil {
		return
	}
	var todo []*frameRun
	for _, f := range frames {
		id, ok := ids[f.Name]
		switch {
		case ok:
			todo = append(todo, &frameRun{f: f, id: id})
		case importErr[f.Path] != "":
			fail(f, "LightCraft couldn't import it: "+importErr[f.Path])
		case cmdErr != "":
			fail(f, "LightCraft import: "+cmdErr)
		case err != nil:
			fail(f, "LightCraft import: "+err.Error())
		default:
			fail(f, "LightCraft didn't import it")
		}
	}
	if len(todo) == 0 {
		return
	}

	// Develop: the presets these frames use, then each frame's steps to its export.
	var cmds []Command
	var presetFiles []string
	seen := map[string]bool{}
	for _, fr := range todo {
		if pr, ok := r.o.Recipe.preset(fr.f); ok && !seen[pr.File] {
			seen[pr.File] = true
			presetFiles = append(presetFiles, filepath.Join(r.presets, pr.File))
		}
	}
	if len(presetFiles) > 0 {
		cmds = append(cmds, Command{"preset.import", map[string]any{"paths": presetFiles}})
	}
	header := len(cmds)
	for _, fr := range todo {
		fr.temp = filepath.Join(r.o.Out, tempPrefix+randHex()+"."+jpegName(fr.f.Name))
		fr.first = len(cmds)
		cmds = append(cmds, r.o.Recipe.Steps(fr.f, fr.id, fr.temp)...)
		fr.end = len(cmds)
	}
	var headerErr string
	at := 0 // todo[at] is the frame the next line belongs to
	_, err = lightcraft(ctx, r.o.LightCraft, lib, dir, "develop", cmds, func(i int, l line) {
		if i < header {
			if !l.OK {
				headerErr = l.Command + ": " + l.errText()
			}
			return
		}
		for at < len(todo) && i >= todo[at].end {
			at++
		}
		if at == len(todo) {
			return
		}
		fr := todo[at]
		fr.ms += l.Ms
		if !l.OK {
			msg := l.Command + ": " + l.errText()
			if l.Command == "preset.apply" && headerErr != "" {
				msg += " (" + headerErr + ")"
			}
			fr.errs = append(fr.errs, msg)
		}
		if i == fr.end-1 { // its export: the frame is finished
			fr.settled = true
			if len(fr.errs) > 0 {
				os.Remove(fr.temp)
				fail(fr.f, strings.Join(fr.errs, "; "))
				return
			}
			if msg := r.place(fr); msg != "" {
				fail(fr.f, msg)
			}
		}
	})
	for _, fr := range todo {
		if fr.settled {
			continue
		}
		os.Remove(fr.temp)
		if ctx.Err() != nil {
			continue // interrupted, not failed: the next run develops it
		}
		why := "exited"
		if err != nil {
			why = err.Error()
		}
		fail(fr.f, "lightcraft-cli stopped before exporting it: "+why)
	}
}

// place checks the frame's temp export and gives it its name: a new name, or over a
// JPEG the state proves cull exported; never over anyone else's. "" = done.
func (r *run) place(fr *frameRun) string {
	b, err := os.ReadFile(fr.temp)
	if err != nil {
		return "reading LightCraft's export: " + err.Error()
	}
	if len(b) < 4 || !bytes.HasPrefix(b, []byte{0xFF, 0xD8}) {
		os.Remove(fr.temp)
		return fmt.Sprintf("LightCraft's export %s isn't a JPEG (%d bytes)", fr.temp, len(b))
	}
	h := sha256.Sum256(b)
	final := filepath.Join(r.o.Out, jpegName(fr.f.Name))
	r.mu.Lock()
	prev := r.st.Frames[fr.f.Name]
	r.mu.Unlock()
	if old, err := os.ReadFile(final); err == nil {
		oh := sha256.Sum256(old)
		if prev == nil || prev.JPEG != final || prev.SHA256 != hex.EncodeToString(oh[:]) {
			os.Remove(fr.temp)
			return final + " exists and isn't a JPEG cull develop exported: move it away and run again"
		}
		if err := os.Rename(fr.temp, final); err != nil {
			os.Remove(fr.temp)
			return err.Error()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		os.Remove(fr.temp)
		return err.Error()
	} else if err := offload.MoveNoReplace(fr.temp, final); err != nil {
		os.Remove(fr.temp)
		return err.Error()
	}
	r.record(fr.f, &FrameState{JPEG: final, Bytes: int64(len(b)), SHA256: hex.EncodeToString(h[:]), Ms: fr.ms}, "")
	return ""
}

// record saves a frame's outcome: done (with its export) or failed (msg). A failed
// frame keeps its previous export's record, so that JPEG stays cull's to replace.
func (r *run) record(f Frame, done *FrameState, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := &FrameState{File: f.Path, Fingerprint: r.p.fps[f.Name], EV: f.EV, At: time.Now().UTC()}
	if pr, ok := r.o.Recipe.preset(f); ok {
		rec.Preset = pr.ID
	}
	if done != nil {
		rec.Status, rec.JPEG, rec.Bytes, rec.SHA256, rec.Ms = statusDone, done.JPEG, done.Bytes, done.SHA256, done.Ms
		r.sum.Developed++
		r.sum.Exports = append(r.sum.Exports, Exported{f.Name, done.JPEG, done.Bytes})
		r.logf("developed %s → %s (%s, %.0f s)\n", f.Name, done.JPEG, sizeText(done.Bytes), done.Ms/1000)
	} else {
		rec.Status, rec.Error = statusFailed, msg
		if prev := r.st.Frames[f.Name]; prev != nil {
			rec.JPEG, rec.Bytes, rec.SHA256 = prev.JPEG, prev.Bytes, prev.SHA256
		}
		r.sum.Failures = append(r.sum.Failures, Failure{f.Name, msg})
		r.logf("failed %s: %s\n", f.Name, msg)
	}
	r.st.Frames[f.Name] = rec
	if err := r.st.save(r.o.StatePath); err != nil && r.saveErr == nil {
		r.saveErr = fmt.Errorf("saving %s: %w", r.o.StatePath, err)
	}
}

func (r *run) logf(format string, a ...any) {
	if r.o.Log != nil {
		fmt.Fprintf(r.o.Log, format, a...)
	}
}

func randHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sizeText is n bytes for people: "3.1 MB".
func sizeText(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f KB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}
