package offload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/cull/internal/ui"
)

// retryDelays are the waits before each retry of a failed file (a flaky reader, a
// card reseated); tests shorten them.
var retryDelays = []time.Duration{time.Second, 3 * time.Second}

// Result is what a run did.
type Result struct {
	Plan       *Plan
	Copied     int
	Skipped    int
	Unverified int      // skipped files never checked against the card (see File.Unverified)
	Failed     []string // "<name>: <error>"
	Bytes      int64
	Dated      int // copies whose capture dates were set (--set-date): patched, or only their file times
	Elapsed    time.Duration
	SyncErrs   []string // destinations whose final F_FULLFSYNC failed
	FsyncOnly  []string // destinations flushed with fsync: F_FULLFSYNC isn't supported there (FsyncOnlyNote)
	// Safe: every planned file is on every destination and was verified against the
	// card (now, or by an earlier run's manifest, or by --checksum), and each
	// destination's drive cache was flushed (on a network share: fsync, see
	// flushDrive). Only then may the card be formatted.
	Safe bool
}

// Run carries out p. Files that fail after retries are listed and the run goes on; a
// cancelled ctx starts no new card read, abandons the file being read (its temp files
// removed), lets the file already in stage B finish (verified in full, or removed),
// and returns what was done with ctx.Err().
//
// Each file goes through two stages (copy.go): A reads the card into temp copies, B
// fsyncs, verifies and names them. While file N is in stage B, file N+1 is in stage A,
// so the card keeps streaming while a destination commits and re-reads: on a NAS the
// fsync alone is a large share of each file's time. Each stage holds one file, so at
// most two are in flight, and stage B taking one file at a time, in plan order, keeps
// the manifest in card order. A file is recorded only once its own stage B passed.
func Run(ctx context.Context, p *Plan, sink ui.Sink) (*Result, error) {
	start := time.Now()
	r := &runner{ctx: ctx, p: p, res: &Result{Plan: p}, sink: sink}
	for _, d := range p.Dests {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return r.res, err
		}
		stale, _ := filepath.Glob(filepath.Join(d, ".*.cull-*.tmp"))
		for _, s := range stale {
			os.Remove(s)
		}
	}
	r.emit(ui.Event{Stage: &ui.Stage{Name: "offload", Unit: "bytes", Total: p.Bytes}})
	defer r.emit(ui.Event{Stage: &ui.Stage{Name: "offload", Done: true}})

	if p.h.serial {
		r.serial()
	} else {
		r.pipeline()
	}
	r.noteSigned()
	res := r.res
	// Only now, with no file in either stage, is each destination's drive cache flushed,
	// once: fsync stops at the drive. That can take a few seconds after a big copy, so
	// it shows as a stage. A network share gets fsync instead (flushDrive).
	ft := ui.Track(sink, "flush", "flushing the drive's write cache", "drives", len(p.Dests))
	for _, d := range p.Dests {
		fsyncOnly, err := flushDrive(d)
		switch {
		case err != nil:
			res.SyncErrs = append(res.SyncErrs, d+": "+err.Error())
			r.warn("couldn't flush %s's drive cache: %v", d, err)
		case fsyncOnly:
			res.FsyncOnly = append(res.FsyncOnly, d)
		}
		ft.Add(1)
	}
	ft.Done()
	res.Elapsed = time.Since(start)
	res.Safe = ctx.Err() == nil && len(res.Failed) == 0 && len(res.SyncErrs) == 0 &&
		res.Unverified == 0 && res.Copied+res.Skipped == len(p.Files)
	return res, ctx.Err()
}

// runner is one Run's state. Only Run's goroutine touches it (stage B's goroutine only
// runs finishStage), so the result, the manifests and the sink need no locks.
type runner struct {
	ctx  context.Context
	p    *Plan
	res  *Result
	sink ui.Sink

	signed     []string        // Content Credentials frames copied with their dates unchanged (--set-date)
	crtimeNote map[string]bool // destinations already noted for a creation-time failure
}

func (r *runner) emit(e ui.Event) {
	if r.sink != nil {
		r.sink.Emit(e)
	}
}

func (r *runner) warn(format string, args ...any) {
	r.emit(ui.Event{Note: &ui.Note{Sev: ui.Warn, Text: fmt.Sprintf(format, args...)}})
}

func (r *runner) verbose(format string, args ...any) {
	r.emit(ui.Event{Note: &ui.Note{Level: ui.Verbose, Text: fmt.Sprintf(format, args...)}})
}

// skip counts a file the plan skips.
func (r *runner) skip(f File) {
	r.res.Skipped++
	if f.Unverified {
		r.res.Unverified++
	}
}

// serial is the engine before the pipeline: each file read, verified and named before
// the next is opened. A test seam (hooks.serial), to compare with the pipeline.
func (r *runner) serial() {
	for i, f := range r.p.Files {
		if f.Skip != "" {
			r.skip(f)
			continue
		}
		if r.ctx.Err() != nil {
			return
		}
		w, err := r.copyWithRetries(f)
		if !r.settle(i, f, w, err) {
			return
		}
	}
}

// inB is the file in stage B: finishStage runs on its own goroutine and closes done.
type inB struct {
	i    int
	f    File
	w    *written
	err  error
	done chan struct{}
}

// pipeline runs stage A of each file on this goroutine while the previous file's
// stage B runs on another. Before acting on file N+1's stage A it waits for file N's
// stage B and settles N (retrying it, recording it, or stopping), so everything is
// settled in plan order and never more than two files are in flight.
func (r *runner) pipeline() {
	var b *inB
	// wait settles the file in stage B, if any; false means stop the run.
	wait := func() bool {
		if b == nil {
			return true
		}
		select {
		case <-b.done:
		case <-r.ctx.Done():
			select {
			case <-b.done:
			default: // Ctrl-C: say why the run doesn't stop at once
				r.emit(ui.Event{Note: &ui.Note{Level: ui.Normal, Text: fmt.Sprintf("finishing %s (already read from the card)…", b.f.Name)}})
				<-b.done
			}
		}
		ok := r.settleStageB(b)
		b = nil
		return ok
	}
	defer wait() // a break with a file still in stage B: wait for it, record it if it passed

	for i, f := range r.p.Files {
		if f.Skip != "" {
			r.skip(f)
			continue
		}
		if r.ctx.Err() != nil {
			return
		}
		w, err := writeStage(r.ctx, f.Src, f.Name, f.To, f.Size, f.ModTime, r.p.h)
		if !wait() {
			if w != nil {
				w.discard() // read and written, but the run stops: never named
			}
			return
		}
		if err != nil {
			// The pipelined read was try 0; the retries are serial, re-reading the card,
			// as copyWithRetries does. Nothing else is in flight meanwhile.
			w, err := r.retryFailed(f, err)
			if !r.settle(i, f, w, err) {
				return
			}
			continue
		}
		if r.ctx.Err() != nil {
			w.discard() // read, but Ctrl-C came before its stage B began: no new work starts
			return
		}
		w.setDate = r.p.setDate
		b = &inB{i: i, f: f, w: w, done: make(chan struct{})}
		go func(b *inB) {
			defer close(b.done)
			// Not cancelled with ctx: this file's card read is complete, so on Ctrl-C it
			// finishes, as it would have before the next card read in a serial run. It
			// still either verifies in full and is named, or is removed.
			b.err = finishStage(context.WithoutCancel(r.ctx), b.w, r.p.h)
		}(b)
	}
}

// settleStageB settles a file whose stage B ended: a failure that a retry might fix is
// retried serially from the card (the pipelined attempt was try 0), while the next
// file, if already read, waits with its temps unsynced and unnamed.
func (r *runner) settleStageB(b *inB) bool {
	w, err := b.w, b.err
	if err != nil {
		w, err = r.retryFailed(b.f, err)
	}
	return r.settle(b.i, b.f, w, err)
}

// settle records file i's outcome; false means stop the run (cancelled, or the
// destination is full). A file that copied and verified is recorded even if ctx was
// cancelled meanwhile: it has its final name, so the manifest must say it verified.
func (r *runner) settle(i int, f File, w *written, err error) bool {
	res := r.res
	if err == nil {
		r.record(f, w)
		return r.ctx.Err() == nil
	}
	if errors.Is(err, context.Canceled) || r.ctx.Err() != nil {
		return false
	}
	if destinationFull(err) {
		res.Failed = append(res.Failed, f.Name+": destination full: "+err.Error())
		r.warn("%s not copied, the destination is full: %v", f.Name, err)
		for _, rest := range r.p.Files[i+1:] {
			if rest.Skip == "" {
				res.Failed = append(res.Failed, rest.Name+": not attempted: destination full")
			}
		}
		return false
	}
	res.Failed = append(res.Failed, f.Name+": "+err.Error())
	r.warn("%s not copied: %v", f.Name, err)
	return true
}

// record writes f's manifest line in each destination and counts it. sha256 is
// always the card's hash; a copy whose dates were patched adds file_sha256 (the copy
// as named) and patched_at, and any copy --set-date dated (patched, or only its file
// times: a Content Credentials frame, or nothing to patch) records dates_set.
func (r *runner) record(f File, w *written) {
	res := r.res
	sum := w.sum
	e := Entry{Src: f.Src, Orig: filepath.Base(f.Src), Name: f.Name, Size: f.Size, ModTime: f.ModTime,
		SHA256: hexOf(sum[:]), At: time.Now().UTC()}
	if w.dated {
		e.DatesSet = w.setDate.Format("2006-01-02T15:04:05")
		if w.patched {
			e.FileSHA256, e.PatchedAt = hexOf(w.fileSum[:]), e.At
		}
	}
	r.noteDates(f, w)
	for _, d := range f.To {
		if err := appendManifest(d, e); err != nil {
			res.Failed = append(res.Failed, f.Name+": manifest: "+err.Error())
			r.warn("%s copied but not recorded in %s: %v", f.Name, d, err)
		}
	}
	if f.Unverified {
		res.Unverified++
	}
	res.Copied++
	if w.dated {
		res.Dated++
	}
	res.Bytes += f.Size
	r.emit(ui.Event{Stage: &ui.Stage{Name: "offload", Add: f.Size}})
	r.emit(ui.Event{Note: &ui.Note{Level: ui.Verbose, Text: fmt.Sprintf("%s → %s (sha256 %s…)", filepath.Base(f.Src), f.Name, hexOf(sum[:4]))}})
}

func (r *runner) copyWithRetries(f File) (*written, error) {
	w, err := copyDated(r.ctx, f.Src, f.Name, f.To, f.Size, f.ModTime, r.p.setDate, r.p.h)
	if err == nil {
		return w, nil
	}
	return r.retryFailed(f, err)
}

// noteDates tells, once per file, what --set-date left alone in it: date values it
// couldn't rewrite at the same length, or the whole file when its metadata can't be
// read. Content Credentials frames are gathered for one note at the end of the run
// (noteSigned); a creation time that can't be set is noted once per destination.
func (r *runner) noteDates(f File, w *written) {
	if w.setDate == nil {
		return
	}
	switch {
	case w.undated != nil:
		r.warn("%s: dates not fixed, its metadata can't be read (%v); copied exactly as the card holds it", f.Name, w.undated)
	case w.c2pa():
		r.signed = append(r.signed, f.Name)
	case len(w.skipped) > 0:
		r.warn("%s: left unchanged (the rest of its dates are set): %s", f.Name, strings.Join(w.skipped, "; "))
	}
	for i, err := range w.crtime {
		if d := f.To[i]; err != nil && !r.crtimeNote[d] {
			if r.crtimeNote == nil {
				r.crtimeNote = map[string]bool{}
			}
			r.crtimeNote[d] = true
			r.warn("couldn't set creation times in %s (%v); modification times are set", d, err)
		}
	}
}

// noteSigned names, in one note per run, the Content Credentials frames --set-date
// copied with their dates unchanged.
func (r *runner) noteSigned() {
	if len(r.signed) == 0 {
		return
	}
	names := strings.Join(r.signed, ", ")
	if n := len(r.signed); n > 5 {
		names = fmt.Sprintf("%s and %d more", strings.Join(r.signed[:5], ", "), n-5)
	}
	r.emit(ui.Event{Note: &ui.Note{Level: ui.Normal, Text: names +
		": Content Credentials — dates left unchanged so the signature stays valid (file times set; the sidecar carries the date)"}})
}

// retryFailed carries on after try 0 of f failed with err: it copies f again with
// copyDated (the plan's --set-date included), from the card, up to len(retryDelays) more times. Before each retry it
// evicts the card file from the page cache, so the retry reads the card, not RAM.
// Nothing else is in flight then, so the eviction can't slow a card read.
func (r *runner) retryFailed(f File, err error) (*written, error) {
	ctx, h := r.ctx, r.p.h
	var w *written
	for try := 0; ; try++ {
		// Retried: card reads and verify mismatches (a flaky reader, a reseated card).
		// Not retried: a full disk or a name taken since planning won't change, and
		// each retry would read the whole file off the card again.
		if err == nil || ctx.Err() != nil || try == len(retryDelays) || destinationFull(err) || errors.Is(err, fs.ErrExist) {
			return w, err
		}
		r.warn("%s: %v; retrying in %s", f.Name, err, retryDelays[try])
		select {
		case <-time.After(retryDelays[try]):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.evictSource(f)
		w, err = copyDated(ctx, f.Src, f.Name, f.To, f.Size, f.ModTime, r.p.setDate, h)
	}
}

// evictSource drops f's card file from the page cache (read-only: evictPasses) before a
// retry. Best effort: if it fails, or pages stay cached, the retry still runs (those
// pages come from RAM, as every retry's did before), with one verbose note.
func (r *runner) evictSource(f File) {
	ev := r.p.h.evictSource
	if ev == nil {
		ev = evictPasses
	}
	switch res, n, err := ev(f.Src); {
	case err != nil:
		r.verbose("%s: couldn't drop the card file from the page cache before retrying (%v); retrying anyway", f.Name, err)
	case res > 0:
		r.verbose("%s: %d of %d pages of the card file stay in the page cache; the retry reads those from RAM", f.Name, res, n)
	}
}

// fullSyncFn and plainSyncFn are the destination flush's syscalls; tests swap them.
var fullSyncFn, plainSyncFn = fullSync, plainSync

// FsyncOnlyNote is the one line a run prints for a destination in Result.FsyncOnly.
func FsyncOnlyNote(dest string) string {
	return dest + ": network share — flushed with fsync (F_FULLFSYNC isn't supported there); the NAS is responsible for its own disk cache"
}

// flushDrive empties dir's drive cache with F_FULLFSYNC. A network share (smbfs on
// macOS returns ENOTSUP, measured 2026-10-06) has no drive here to flush: it falls
// back to fsync on the folder, and fsyncOnly says so. That counts as flushed even if
// the share refuses fsync on a folder too, because finishStage fsyncs every file before
// its verify read and fails the file if that fsync fails: the server has acknowledged
// every file's data, the most a share can promise. Any other error is a failure.
func flushDrive(dir string) (fsyncOnly bool, err error) {
	d, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer d.Close()
	if err := fullSyncFn(d); !unsupported(err) {
		return false, err
	}
	if err := plainSyncFn(d); err != nil && !unsupported(err) {
		return false, err
	}
	return true, nil
}

// unsupported reports a sync the filesystem doesn't implement (ENOTSUP and
// EOPNOTSUPP differ on darwin).
func unsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

// Verified is what Verify found.
type Verified struct {
	OK, Bad    int
	Unrecorded []string // DNGs in the folder that no manifest line covers: never verified by cull
}

// Verify re-hashes every file folder's manifest records, from the disk, and reports
// how many match and how many are missing or differ (each with a warning). Each file
// is checked under the name and checksum of its current (last) entry: file_sha256 for
// a file whose dates were patched, the card's sha256 otherwise.
func Verify(ctx context.Context, folder string, sink ui.Sink) (v Verified, err error) {
	ok, bad := 0, 0
	defer func() { v.OK, v.Bad = ok, bad }()
	man, err := readManifest(folder)
	if err != nil {
		return v, err
	}
	if len(man) == 0 {
		return v, fmt.Errorf("no %s in %s: nothing to verify", ManifestName, folder)
	}
	// Each file is checked against its current entry: redate and rename append lines
	// that supersede earlier ones (new name, patched checksum).
	man = current(man)
	if sink != nil {
		sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Unit: "files", Total: int64(len(man))}})
		defer sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Done: true}})
	}
	for _, e := range man {
		if err := ctx.Err(); err != nil {
			return v, err
		}
		p := locate(folder, e.Name)
		problem := ""
		if st, err := os.Stat(p); err != nil {
			problem = "missing"
		} else if st.Size() != e.Size {
			problem = fmt.Sprintf("size %d, the card's was %d", st.Size(), e.Size)
		} else if sum, err := hashFromDisk(ctx, p); err != nil {
			problem = err.Error()
		} else if hexOf(sum[:]) != e.fileSHA() {
			problem = "differs from the card (checksum mismatch)"
			if e.FileSHA256 != "" {
				problem = "differs from the recorded checksum (its dates were patched)"
			}
		}
		if problem != "" {
			bad++
			if sink != nil {
				sink.Emit(ui.Event{Note: &ui.Note{Sev: ui.Warn, Text: e.Name + ": " + problem}})
			}
		} else {
			ok++
		}
		if sink != nil {
			sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Add: 1}})
		}
	}
	recorded := map[string]bool{}
	for _, e := range man {
		recorded[e.Name] = true
	}
	for _, sub := range append([]string{""}, MovedDirs...) {
		ents, err := os.ReadDir(filepath.Join(folder, sub))
		if err != nil {
			if sub == "" {
				return v, err
			}
			continue
		}
		for _, d := range ents {
			// Dot-files are skipped as the card walk skips them: macOS writes AppleDouble
			// "._" files beside frames on exFAT when they're opened.
			if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") && strings.EqualFold(filepath.Ext(d.Name()), ".dng") && !recorded[d.Name()] {
				v.Unrecorded = append(v.Unrecorded, filepath.Join(sub, d.Name()))
			}
		}
	}
	return v, nil
}

// destinationFull reports a write that failed for lack of space: every later file
// would fail the same way, after reading it off the card for nothing.
func destinationFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// MovedDirs are the folders, inside a shoot folder, that cull moves frames into:
// --move-culled's culled/ and --sort's keep/, review/, cull/ (pipeline's placeDirs; a
// test there keeps the two lists the same). Verify finds copies there too.
var MovedDirs = []string{"culled", "keep", "review", "cull"}

// locate is where a recorded copy is now: in the shoot folder, or moved into one of
// MovedDirs (the first that has it).
func locate(folder, name string) string {
	p := filepath.Join(folder, name)
	if _, err := os.Lstat(p); err == nil {
		return p
	}
	for _, d := range MovedDirs {
		if q := filepath.Join(folder, d, name); fileExists(q) {
			return q
		}
	}
	return p
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }
