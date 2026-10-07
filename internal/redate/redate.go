// Package redate fixes the capture dates of frames already offloaded (cull redate):
// for a camera whose clock stopped. Each DNG's date fields are rewritten at the same
// length into a hidden temp beside it, the temp is proven byte for byte to be the
// original with exactly those patches, and only then renamed over the original: the
// one place cull replaces a file. A file that no longer matches the checksum its
// offload manifest records is refused, never "fixed". The manifest, the report's
// keys and cull's sidecars follow every file, so the next judge re-bills nothing, and
// a journal (cull-redate.json) lets an interrupted run be finished by running it again.
package redate

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
	"github.com/jefflaplante/cull/internal/xmp"
)

// Options are one redate run.
type Options struct {
	Dir        string
	ReportPath string // "" = <Dir>/cull-report.json
	Recursive  bool
	Target     time.Time // local
	DryRun     bool
	UI         ui.Sink
}

// Result is what a run did (with DryRun: would do).
type Result struct {
	Patched    int // date fields rewritten, proven and swapped in; file times set
	TimesOnly  int // nothing to patch (Content Credentials, or the fields already held the date): file times set
	AlreadySet int
	Refused    int
	Refusals   []string // "<name>: <why>"
	Skipped    []string // "<name>: <field>": values left alone (unparseable, or a length change)
	Signed     []string // Content Credentials frames: their bytes and signed dates unchanged
	// Orphans are hidden redate temps left alone that may be a frame's only copy (their
	// file is missing, or isn't what the journal recorded): the user decides.
	Orphans []string
	// Interrupted counts swaps that failed after the original's name was gone: the
	// proven temp holds the frame, and the next run restores it.
	Interrupted int
	// CrtimeFailed counts files whose creation time couldn't be set (best effort).
	CrtimeFailed int
}

// checkpointN is how many recorded files the report and journal may lag behind, as
// judge's checkpoint does.
const checkpointN = 25

const isoLocal = "2006-01-02T15:04:05"

// crashAfterSwap is a test seam: true stops Run right after that file's swap, before
// its manifest line, report save or sidecar, as a crash would.
var crashAfterSwap func(path string) bool

// trace is a test seam: the durability steps, in order.
var trace func(event string)

var errCrash = errors.New("redate: stopped right after a swap (test)")

type run struct {
	ctx                        context.Context
	o                          Options
	dir, reportPath, targetISO string
	res                        Result
	rep                        *report.Report
	lab                        map[string]labels.Entry
	man                        map[string]map[string]offload.Entry // shoot folder → lower-case name → current entry
	j                          *journal.Redate                     // nil on a dry run
	done                       map[string]bool                     // j.Done as a set
	unsaved                    []string                            // recorded since the last checkpoint
	dirty                      bool                                // the report changed since the last save
	manDirty                   map[string]bool                     // shoot folders whose manifest grew since the last checkpoint
	xattrNoted                 map[string]bool                     // extended-attribute failures noted, by kind
	warned                     bool                                // the catalogue warning was given
	prev                       *journal.Redate                     // a dry run's view of the unfinished journal (read only)
	held                       map[string]bool                     // files with a hidden temp left alone: not touched this run
	unrecordedSigned           int                                 // Content Credentials frames with no manifest line or report entry
}

// changing gives, once, before the first file changes, the warning every command that
// changes files after import gives.
func (r *run) changing() {
	if !r.warned {
		r.warned = true
		r.warn("if these frames are already in a Capture One or Lightroom catalogue, it may lose track of the changed files")
	}
}

// Run fixes the capture dates of every DNG in o.Dir (and its keep/, review/, cull/ and
// culled/ folders; subfolders with Recursive) to o.Target. A refused file is counted
// and listed, and the run goes on; an error stops it (the journal stays, so running
// it again finishes it).
func Run(ctx context.Context, o Options) (Result, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return Result{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Result{}, fmt.Errorf("%s is not a directory", o.Dir)
	}
	r := &run{ctx: ctx, o: o, dir: dir, reportPath: o.ReportPath, targetISO: o.Target.Format(isoLocal)}
	defReport := filepath.Join(dir, "cull-report.json")
	if r.reportPath == "" {
		r.reportPath = defReport
	}
	if slices.Contains(offload.MovedDirs, filepath.Base(dir)) {
		return r.res, fmt.Errorf("%s is a sort folder: run cull redate on its shoot folder, %s", dir, filepath.Dir(dir))
	}
	if !o.DryRun {
		release, note, err := journal.Lock(dir, true, "redate")
		if err != nil {
			return r.res, err
		}
		defer release()
		if note != "" {
			r.warn("%s", note)
		}
	}
	for _, p := range journal.Unfinished(dir) {
		if p.Which != "redate" {
			return r.res, fmt.Errorf("an unfinished %s is recorded in %s: finish it first with %s", p.Which, dir, p.Finish)
		}
	}
	if err := journal.PendingBatch(dir, r.reportPath); err != nil {
		return r.res, err
	}
	j, err := journal.LoadRedate(dir)
	if err != nil {
		return r.res, fmt.Errorf("%w: move it aside if no redate is running", err)
	}
	reportField := "" // the journal's Report: "" for the folder's own report
	if r.reportPath != defReport {
		reportField = r.reportPath
	}
	if j != nil && !j.Complete {
		if j.Target != r.targetISO {
			return r.res, fmt.Errorf("an unfinished redate to %s is recorded in %s: finish it first with %s",
				strings.Replace(j.Target, "T", " ", 1), filepath.Join(dir, journal.RedateName), j.Finish(dir))
		}
		// Its records are relative to its folders and its report: finish it with those.
		if j.Recursive != o.Recursive || j.Report != reportField {
			return r.res, fmt.Errorf("the unfinished redate recorded in %s ran with recursive %v and report %s: finish it with %s",
				filepath.Join(dir, journal.RedateName), j.Recursive, cmp.Or(j.Report, defReport), j.Finish(dir))
		}
	}
	folders, err := Folders(dir, o.Recursive, "redate")
	if err != nil {
		return r.res, err
	}
	// Everything the run updates is read first: a file is never changed while its
	// bookkeeping can't follow.
	switch rep, err := report.Load(r.reportPath); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return r.res, fmt.Errorf("%s can't be read (%v): its keys must follow the files, so nothing was changed", r.reportPath, err)
	default:
		rep.Relocate(r.reportPath, dir)
		r.rep = rep
		if r.lab, err = labels.Read(labels.DefaultPath(r.reportPath)); err != nil {
			return r.res, err
		}
	}
	r.man = map[string]map[string]offload.Entry{}
	r.manDirty = map[string]bool{}
	for _, d := range folders {
		folder := folderShoot(dir, d)
		if _, ok := r.man[folder]; ok {
			continue
		}
		es, err := offload.CurrentManifest(folder)
		if err != nil {
			return r.res, fmt.Errorf("%s: %w", filepath.Join(folder, offload.ManifestName), err)
		}
		m := map[string]offload.Entry{}
		for _, e := range es {
			m[strings.ToLower(e.Name)] = e
		}
		r.man[folder] = m
	}

	if o.DryRun {
		if j != nil && !j.Complete { // read only: what a re-run would restore
			r.prev = j
		}
	} else {
		if j == nil || j.Complete {
			j = &journal.Redate{Target: r.targetISO, Started: time.Now(), Recursive: o.Recursive, Report: reportField}
		}
		if j.Files == nil {
			j.Files = map[string]journal.FileState{}
		}
		r.done = map[string]bool{}
		for _, d := range j.Done {
			r.done[d] = true
		}
		r.j = j
		if err := r.saveJournal(); err != nil {
			return r.res, err
		}
	}
	for _, d := range folders {
		if err := r.settleTemps(d); err != nil {
			return r.res, err
		}
	}
	files, err := listDNGs(folders)
	if err != nil {
		return r.res, err
	}

	t := ui.Track(o.UI, "redate", "fixing capture dates", "files", len(files))
	var runErr error
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			runErr = err
			break
		}
		if err := r.file(p); err != nil {
			if errors.Is(err, errCrash) {
				t.Done()
				return r.res, err
			}
			runErr = err
			break
		}
		t.Add(1)
		if len(r.unsaved) >= checkpointN {
			if err := r.checkpoint(); err != nil {
				runErr = err
				break
			}
		}
	}
	t.Done()
	if !o.DryRun {
		if err := r.checkpoint(); err != nil && runErr == nil {
			runErr = err
		}
		switch {
		case runErr != nil:
		case len(r.res.Orphans) > 0: // a re-run can't settle them: the user decides
			r.warn("%d hidden temp file(s) left alone may be a frame's only copy: deal with each as said above; until then the journal stays, and redate finishes the run once they're settled", len(r.res.Orphans))
		case len(r.j.Files) > 0: // journalled and not finished
			r.warn("%d file(s) may be half done: run the same redate again to finish them", len(r.j.Files))
		default:
			r.traced("journal remove")
			if err := journal.RemoveRedate(dir); err != nil {
				runErr = err
			}
		}
		r.noteSigned()
	}
	r.noteUnrecorded()
	return r.res, runErr
}

// file fixes one DNG. An error stops the run; a refusal is counted and returns nil.
func (r *run) file(p string) error {
	rel, _ := filepath.Rel(r.dir, p)
	if r.held[rel] { // its temp was left alone, and its journal record stays with it
		if r.o.DryRun {
			r.say("%s: would be left alone with the hidden temp beside it (see above)", rel)
			return nil
		}
		r.verbose("%s: left alone with the hidden temp beside it (see above)", rel)
		return nil
	}
	st, err := os.Lstat(p)
	if err != nil {
		r.refuse(rel, err.Error())
		return nil
	}
	ps, skipped, before, err := patchesFor(p, st.Size(), r.o.Target)
	if err != nil {
		r.refuse(rel, "its dates can't be read: "+err.Error())
		return nil
	}
	signed := len(skipped) == 1 && skipped[0] == dng.ContentCredentialsSkip
	if signed {
		r.res.Signed = append(r.res.Signed, rel)
	} else if len(skipped) > 0 {
		for _, s := range skipped {
			r.res.Skipped = append(r.res.Skipped, rel+": "+s)
		}
		r.warn("%s: left unchanged (the rest of its dates are set): %s", rel, strings.Join(skipped, "; "))
	}
	folder := shootFolder(r.dir, p)
	e, recorded := r.man[folder][strings.ToLower(filepath.Base(p))]
	if recorded && e.Size != st.Size() {
		r.refuse(rel, fmt.Sprintf("%v (%d bytes, recorded %d)", offload.ErrChanged, st.Size(), e.Size))
		return nil
	}
	if signed && !recorded && (r.rep == nil || r.rep.ResultFor(p) == nil) {
		r.unrecordedSigned++ // its corrected date can't be recorded anywhere (see noteUnrecorded)
	}
	// The camera's time, which grouping keeps reading: the manifest's when it records
	// one (never replaced), else the time these patches replace, when there are any.
	camera := ""
	if recorded {
		camera = e.CameraTime
	}
	if camera == "" && len(ps) > 0 {
		camera = before
	}
	when := r.o.Target.Format(time.DateTime)
	if len(ps) == 0 && r.isTarget(st.ModTime()) {
		r.res.AlreadySet++
		if r.o.DryRun {
			r.say("%s: already set", rel)
			return nil
		}
		return r.recover(p, rel, st, folder, e, recorded)
	}
	if r.o.DryRun {
		switch {
		case signed:
			r.res.TimesOnly++
			r.say("%s: Content Credentials, dates kept; file times → %s", rel, when)
		case len(ps) == 0:
			r.res.TimesOnly++
			r.say("%s: file times → %s", rel, when)
		default:
			r.res.Patched++
			r.say("%s: %d fields → %s", rel, len(ps), when)
		}
		return nil
	}

	pre := journal.FileState{Size: st.Size(), ModTime: st.ModTime(), CameraTime: camera}
	if len(ps) == 0 {
		// Its bytes don't change, but it is still proven first, so a damaged frame is
		// refused and left exactly as it is, as a patched one would be: against the
		// journal when an earlier run of this redate swapped it in (its mtime didn't
		// stick), else against its manifest.
		want := ""
		rec, journaled := r.j.Files[rel]
		switch {
		case journaled && rec.Want != "":
			if why, err := r.prove(p, rec.Want, "the journal's", "its dates are set, but it isn't the file redate wrote"); err != nil || why != "" {
				return r.refused(rel, why, err)
			}
			pre, want = rec, rec.Want // the report follows from the original's key
		case recorded:
			if why, err := r.prove(p, e.CurrentSHA256(), "its manifest", offload.ErrChanged.Error()); err != nil || why != "" {
				return r.refused(rel, why, err)
			}
		}
		r.changing()
		r.j.Files[rel] = pre
		if err := r.saveJournal(); err != nil {
			return err
		}
		crErr, err := offload.SetFileTimes(p, r.o.Target)
		if err != nil { // the mtime didn't change: nothing more to finish than before
			if rec, ok := r.j.Files[rel]; ok && rec.Want == "" {
				delete(r.j.Files, rel)
			}
			r.refuse(rel, "its times can't be set: "+err.Error())
			return nil
		}
		r.crtime(crErr)
		r.res.TimesOnly++
		r.verbose("%s: file times → %s", rel, when)
		return r.record(p, rel, pre, folder, e, recorded, want, camera)
	}
	expect := ""
	if recorded {
		expect = e.CurrentSHA256()
	}
	r.changing()
	var journalErr error
	rp, err := offload.ReplacePatched(r.ctx, p, ps, expect, r.o.Target, func(orig, want [32]byte) error {
		pre.Orig, pre.Want = hex.EncodeToString(orig[:]), hex.EncodeToString(want[:])
		r.j.Files[rel] = pre
		journalErr = r.saveJournal()
		return journalErr
	})
	if err != nil && !rp.Swapped && !rp.TempKept { // the original is untouched: nothing to finish
		delete(r.j.Files, rel)
	}
	switch {
	case journalErr != nil:
		return journalErr
	case err != nil && rp.TempKept && rp.Proven: // the original's name went before the swap finished
		r.res.Interrupted++
		r.warn("%s: replacement interrupted — run redate again to restore it from its proven temp (%v)", rel, err)
		return nil
	case err != nil && rp.TempKept: // the original vanished before the temp was proven
		r.orphan(rp.Temp, p, fmt.Sprintf("%s vanished while it was being fixed (%v); the hidden temp is an unproven, possibly partial copy", rel, err))
		return nil
	case errors.Is(err, offload.ErrChanged):
		r.refuse(rel, err.Error())
		return nil
	case err != nil && r.ctx.Err() != nil && !rp.Swapped:
		return r.ctx.Err()
	case err != nil: // swapped but its folder's flush failed: the journal keeps it for a re-run
		r.refuse(rel, err.Error())
		return nil
	}
	r.crtime(rp.CrtimeErr)
	r.xattrs(rel, rp.XattrErrs)
	r.res.Patched++
	r.verbose("%s: %d fields → %s (proven)", rel, len(ps), when)
	if crashAfterSwap != nil && crashAfterSwap(p) {
		return errCrash
	}
	return r.record(p, rel, pre, folder, e, recorded, pre.Want, camera)
}

// record does a changed file's bookkeeping: a superseding manifest line (want: the
// patched checksum, "" when only its times changed; camera: the camera's capture
// time), its report entry, its sidecar.
func (r *run) record(p, rel string, pre journal.FileState, folder string, e offload.Entry, recorded bool, want, camera string) error {
	now, err := os.Stat(p)
	if err != nil {
		r.refuse(rel, "after the change: "+err.Error())
		return nil
	}
	if recorded {
		if err := r.supersede(folder, e, want, camera); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
	}
	r.follow(p, pre, now, camera)
	r.unsaved = append(r.unsaved, rel)
	return nil
}

// recover is an already-set file's bookkeeping, done only where it's missing: after an
// interruption between a swap and its records, or a frame whose dates were set some
// other way. Before vouching for a file it hasn't just written, it proves it: against
// the journal's patched checksum when this run's journal changed it, else against the
// manifest's checksum.
func (r *run) recover(p, rel string, st os.FileInfo, folder string, e offload.Entry, recorded bool) error {
	fs, journaled := r.j.Files[rel]
	camera := cmp.Or(e.CameraTime, fs.CameraTime) // e's when recorded: never replaced
	needMan := recorded && (e.DatesSet != r.targetISO || (journaled && fs.Want != "" && e.FileSHA256 != fs.Want) ||
		(e.CameraTime == "" && camera != ""))
	var x *report.Result
	if r.rep != nil {
		x = r.rep.ResultFor(p)
	}
	current := x != nil && x.Size == st.Size() && x.ModTime.Equal(st.ModTime())
	follows := x != nil && journaled && x.Size == fs.Size && x.ModTime.Equal(fs.ModTime)
	needRep := (current && x.DatesSet != r.targetISO) || follows
	if !needMan && !needRep {
		if journaled {
			r.unsaved = append(r.unsaved, rel) // its record is cleared at the checkpoint
		}
		return nil
	}
	want := ""
	switch {
	case journaled && fs.Want != "":
		if why, err := r.prove(p, fs.Want, "the journal's", "its dates are set, but it isn't the file redate wrote"); err != nil || why != "" {
			return r.refused(rel, why, err)
		}
		want = fs.Want
	case needMan:
		if why, err := r.prove(p, e.CurrentSHA256(), "its manifest", offload.ErrChanged.Error()); err != nil || why != "" {
			return r.refused(rel, why, err)
		}
	}
	if needMan {
		if err := r.supersede(folder, e, want, camera); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
	}
	pre := journal.FileState{Size: st.Size(), ModTime: st.ModTime()}
	if follows {
		pre = fs
	}
	r.follow(p, pre, st, camera)
	r.unsaved = append(r.unsaved, rel)
	return nil
}

// supersede appends e's superseding manifest line: the card's fields and checksum
// unchanged, plus the date set and, for a patched file, its checksum now. The camera's
// capture time is recorded once, the first time (camera), and carried after that.
func (r *run) supersede(folder string, e offload.Entry, want, camera string) error {
	ne := e
	ne.DatesSet = r.targetISO
	if ne.CameraTime == "" {
		ne.CameraTime = camera
	}
	if want != "" {
		ne.FileSHA256, ne.PatchedAt = want, time.Now().UTC()
	}
	if err := offload.AppendManifest(folder, ne); err != nil {
		return err
	}
	r.man[folder][strings.ToLower(e.Name)] = ne
	r.manDirty[folder] = true
	return nil
}

// follow moves the report entry for p to the file's new version (now) when it
// described the version before the change (pre), or already describes now; one that
// describes some other version is left alone (judge redoes it anyway). It records the
// date set and rewrites cull's sidecar.
//
// The entry's Exif is left as the camera recorded it and as the frame was judged:
// sequence grouping reads capture times from it, so rewriting it would regroup the
// shoot's sets on the next judge or decide (and judge would pay to rank them again).
// The corrected date lives in DatesSet, which the sidecar uses, and the camera's time
// in CameraTime (camera, recorded once), which grouping reads over Exif's.
func (r *run) follow(p string, pre journal.FileState, now os.FileInfo, camera string) {
	if r.rep == nil {
		return
	}
	x := r.rep.ResultFor(p)
	switch {
	case x == nil:
		return
	case x.Size == pre.Size && x.ModTime.Equal(pre.ModTime):
		x.ModTime = now.ModTime()
	case x.Size == now.Size() && x.ModTime.Equal(now.ModTime()):
	default:
		return
	}
	x.DatesSet = r.targetISO
	if x.CameraTime == "" {
		x.CameraTime = camera
	}
	r.dirty = true
	at := x.File
	if x.MovedTo != "" {
		at = x.MovedTo
	}
	if !xmp.Ours(xmp.Path(at)) { // none, or not cull's: foreign sidecars are never touched
		return
	}
	if err := labels.WriteSidecar(x, r.lab[filepath.Base(x.File)], x.XMPDevelop, false, r.rep.Tags); err != nil {
		r.warn("%s: sidecar not updated: %v", filepath.Base(at), err)
	}
}

// checkpoint carries what the recorded files' bookkeeping wrote to the media (the
// manifest lines; the report, saved, with its folder), and only then saves the journal
// without their records: a crash can't leave a cleared record whose report is lost.
func (r *run) checkpoint() error {
	for folder := range r.manDirty {
		if err := r.flush(filepath.Join(folder, offload.ManifestName)); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
		delete(r.manDirty, folder)
	}
	if r.rep != nil && r.dirty {
		if err := r.rep.Save(r.reportPath); err != nil {
			return fmt.Errorf("report: %w", err)
		}
		for _, p := range []string{r.reportPath, filepath.Dir(r.reportPath)} {
			if err := r.flush(p); err != nil {
				return fmt.Errorf("report: %w", err)
			}
		}
		r.dirty = false
	}
	for _, rel := range r.unsaved {
		delete(r.j.Files, rel)
		if !r.done[rel] {
			r.done[rel] = true
			r.j.Done = append(r.j.Done, rel)
		}
	}
	r.unsaved = nil
	return r.saveJournal()
}

func (r *run) saveJournal() error {
	r.traced("journal save")
	if err := r.j.Save(r.dir); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

func (r *run) flush(p string) error {
	r.traced("flush " + p)
	return offload.Flush(p)
}

func (r *run) traced(ev string) {
	if trace != nil {
		trace(ev)
	}
}

// xattrs notes, once per kind of failure, extended attributes the swap couldn't keep.
func (r *run) xattrs(rel string, errs []error) {
	for _, err := range errs {
		kind := err
		for errors.Unwrap(kind) != nil {
			kind = errors.Unwrap(kind)
		}
		if r.xattrNoted == nil {
			r.xattrNoted = map[string]bool{}
		}
		if !r.xattrNoted[kind.Error()] {
			r.xattrNoted[kind.Error()] = true
			r.warn("%s: %v (dates fixed; later files with the same failure aren't listed)", rel, err)
		}
	}
}

// patchesFor gives p's date patches to t and, when there are any, the capture time
// they replace (dng.Exif.CameraTime of p as it is).
func patchesFor(p string, size int64, t time.Time) ([]dng.Patch, []string, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, "", err
	}
	defer f.Close()
	ps, skipped, err := dng.PatchDates(f, size, t)
	if err != nil || len(ps) == 0 {
		return ps, skipped, "", err
	}
	ex, err := dng.ReadExifFrom(f, size)
	if err != nil {
		return nil, nil, "", err
	}
	return ps, skipped, ex.CameraTime(), nil
}

// Folders lists dir, its sort folders (offload.MovedDirs), and with recursive every
// subfolder: the folders cull redate (and cull rename) change files in. Hidden folders
// are skipped. A subfolder holding its own report or offload manifest is another shoot
// folder, with its own bookkeeping: refused, naming cmd ("redate", "rename").
func Folders(dir string, recursive bool, cmd string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch {
		case p == dir:
		case strings.HasPrefix(d.Name(), "."):
			return filepath.SkipDir
		case slices.Contains(offload.MovedDirs, d.Name()): // the shoot's sorted frames
		case !recursive:
			return filepath.SkipDir
		default:
			for _, own := range []string{"cull-report.json", offload.ManifestName} {
				if _, err := os.Stat(filepath.Join(p, own)); err == nil {
					return fmt.Errorf("%s is a shoot folder of its own (it has %s): run cull %s on each shoot folder, not -r on %s", p, own, cmd, dir)
				}
			}
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// listDNGs lists the DNGs in folders (not below them). Hidden files are skipped.
func listDNGs(folders []string) ([]string, error) {
	var out []string
	for _, d := range folders {
		ents, err := os.ReadDir(d)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") && strings.EqualFold(filepath.Ext(e.Name()), ".dng") {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// settleTemps deals with the redate temps a crash left in folder d.
//   - Beside its file: a swap that never happened, removed. When the journal records
//     the file, only once the file proves to be what it recorded (before or after the
//     patches); otherwise both stay, and the user is told.
//   - Its file missing: it may be that file's only copy (a rename-over that deletes
//     first, interrupted). If the journal records the file and the temp proves against
//     its patched checksum, it takes the file's name; otherwise it is left alone, and the
//     user is told how to put it back.
//
// A dry run changes nothing: it reads the files to say what a run would do, and holds
// (reports, and leaves out) the frames a run would leave alone.
func (r *run) settleTemps(d string) error {
	jr := r.j
	if jr == nil {
		jr = r.prev // a dry run reads the unfinished journal, changing nothing
	}
	for tmp, target := range offload.RedateTemps(d) {
		rel, _ := filepath.Rel(r.dir, target)
		var rec journal.FileState
		journaled := false
		if jr != nil {
			rec, journaled = jr.Files[rel]
		}
		if tst, err := os.Lstat(target); err == nil {
			keep, err := r.keepBeside(tmp, target, rel, tst, rec, journaled)
			if err != nil {
				return err
			}
			switch {
			case keep != "":
				r.orphan(tmp, target, keep)
			case r.o.DryRun:
				r.verbose("%s: a run would remove the hidden temp %s beside it (a swap that never happened)", rel, tmp)
			default:
				os.Remove(tmp)
			}
			continue
		}
		if !journaled || rec.Want == "" {
			r.orphan(tmp, target, rel+" is missing")
			continue
		}
		why, err := r.prove(tmp, rec.Want, "the journal's", "it isn't what redate wrote")
		if err != nil {
			return err
		}
		if why != "" {
			r.orphan(tmp, target, rel+" is missing, and the temp doesn't prove: "+why)
			continue
		}
		if r.o.DryRun {
			r.say("%s: a re-run would restore it from its proven temp (an interrupted swap)", rel)
			continue
		}
		if err := offload.Adopt(tmp, target); err != nil {
			return fmt.Errorf("restore %s from %s: %w", rel, filepath.Base(tmp), err)
		}
		r.warn("%s: restored from its proven temp (an interrupted swap)", rel)
	}
	return nil
}

// keepBeside decides about a redate temp beside its file: "" removes it (a swap that
// never happened, the file intact; or a partial copy), else why both stay. It only
// reads.
//   - A journalled file must be what the journal recorded, before or after its patches.
//     If it isn't, the temp still goes when it is a partial copy: shorter than the file
//     and than the file the journal recorded, and not the journal's patched file.
//   - Otherwise a temp shorter than the frame's full size is a partial copy: the size its
//     manifest records (never the size of whatever now sits at the name: a larger
//     foreign file there mustn't cost the only copy), or with no manifest, the file's. A
//     full one goes only if the file proves against its manifest.
func (r *run) keepBeside(tmp, target, rel string, tst os.FileInfo, rec journal.FileState, journaled bool) (string, error) {
	if journaled {
		sum, err := offload.HashFromDisk(r.ctx, target)
		switch {
		case err != nil && r.ctx.Err() != nil:
			return "", r.ctx.Err()
		case err != nil:
			return fmt.Sprintf("%s can't be read to check it against the journal (%v)", rel, err), nil
		}
		if h := hex.EncodeToString(sum[:]); h != rec.Orig && h != rec.Want {
			partial, err := r.partialTemp(tmp, tst.Size(), rec)
			if err != nil || partial {
				return "", err
			}
			return rel + " isn't what the journal recorded, before or after its patches", nil
		}
		return "", nil
	}
	e, recorded := r.man[shootFolder(r.dir, target)][strings.ToLower(filepath.Base(target))]
	full := tst.Size()
	if recorded {
		full = e.Size
	}
	if st, err := os.Lstat(tmp); err == nil && st.Size() < full {
		return "", nil // cut short: a partial copy, never a frame
	}
	if !recorded {
		return rel + " has no record to check it against, and the temp is as large as it", nil
	}
	why, err := r.prove(target, e.CurrentSHA256(), "its manifest", "it doesn't match its manifest")
	if err != nil {
		return "", err
	}
	if why != "" {
		return rel + ": " + why, nil
	}
	return "", nil
}

// partialTemp reports a temp beside a journalled frame that can't be the good copy: it
// is shorter than the frame and than the file the journal recorded (patches keep the
// length), and it doesn't prove against the journal's patched checksum. A temp that
// can't be read, or a record without a size, says no.
func (r *run) partialTemp(tmp string, frameSize int64, rec journal.FileState) (bool, error) {
	st, err := os.Lstat(tmp)
	if err != nil || rec.Size <= 0 || st.Size() >= frameSize || st.Size() >= rec.Size {
		return false, nil
	}
	w, derr := hex.DecodeString(rec.Want)
	if derr != nil || len(w) != 32 {
		return true, nil // nothing it could be: shorter than the file recorded
	}
	switch err := offload.ProveFrom(r.ctx, tmp, [32]byte(w)); {
	case err == nil:
		return false, nil
	case r.ctx.Err() != nil:
		return false, r.ctx.Err()
	default:
		return errors.Is(err, offload.ErrNotProven), nil
	}
}

// orphan reports a redate temp left alone: it may be a frame's only copy.
func (r *run) orphan(tmp, target, why string) {
	r.res.Orphans = append(r.res.Orphans, tmp)
	if r.held == nil {
		r.held = map[string]bool{}
	}
	rel, _ := filepath.Rel(r.dir, target)
	r.held[rel] = true
	if _, err := os.Lstat(target); err == nil {
		left := "was"
		if r.o.DryRun {
			left = "would be"
		}
		r.warn("%s; the hidden temp %s %s left alone: compare the two and keep the frame (delete the other)", why, tmp, left)
		return
	}
	r.warn("%s; the hidden temp %s may be its only copy. If it is the frame, put it back with: mv -n %s %s (if it isn't, delete it)",
		why, tmp, journal.ShellQuote(tmp), journal.ShellQuote(target))
}

// prove checks p's bytes, read from the disk, against sum (hex; whose names its
// source, e.g. "its manifest"). It returns the refusal ("" when p matches): mismatch
// for different bytes, or a read error with retry advice. err is a cancellation.
func (r *run) prove(p, sum, whose, mismatch string) (why string, err error) {
	w, derr := hex.DecodeString(sum)
	if derr != nil || len(w) != 32 {
		return whose + " checksum can't be read", nil
	}
	switch perr := offload.ProveFrom(r.ctx, p, [32]byte(w)); {
	case perr == nil:
		return "", nil
	case r.ctx.Err() != nil:
		return "", r.ctx.Err()
	case errors.Is(perr, offload.ErrNotProven):
		return mismatch, nil
	default:
		return fmt.Sprintf("it can't be read to check it (%v); run redate again", perr), nil
	}
}

// refused is prove's outcome as file's: a refusal (nil), or the cancellation.
func (r *run) refused(rel, why string, err error) error {
	if err != nil {
		return err
	}
	r.refuse(rel, why)
	return nil
}

// isTarget reports a modification time that is the target, within FAT's 2 s
// resolution: FAT32 stores an odd second rounded, and every run would otherwise set
// those frames' times again. (On a finer filesystem a frame within 2 s of the target
// counts as set too.)
func (r *run) isTarget(m time.Time) bool {
	d := m.Sub(r.o.Target)
	return d >= -2*time.Second && d <= 2*time.Second
}

// ShootOf is the folder whose manifest records the files in d (one of Folders(root,
// …)): d, or the one above a sort folder.
func ShootOf(root, d string) string { return folderShoot(root, d) }

// folderShoot is the folder whose manifest records the files in d: d, or the one
// above a sort folder.
func folderShoot(root, d string) string {
	if d != root && slices.Contains(offload.MovedDirs, filepath.Base(d)) {
		return filepath.Dir(d)
	}
	return d
}

// shootFolder is the folder whose manifest records p: its own, or the one above a
// sort folder.
func shootFolder(root, p string) string { return folderShoot(root, filepath.Dir(p)) }

func (r *run) refuse(rel, why string) {
	r.res.Refused++
	r.res.Refusals = append(r.res.Refusals, rel+": "+why)
	if r.o.DryRun {
		r.say("%s: would refuse: %s", rel, why)
		return
	}
	r.warn("%s: not changed: %s", rel, why)
}

func (r *run) crtime(err error) {
	if err == nil {
		return
	}
	if r.res.CrtimeFailed == 0 {
		r.warn("couldn't set creation times (%v); modification times are set", err)
	}
	r.res.CrtimeFailed++
}

// noteUnrecorded says, once, how many Content Credentials frames had nowhere to record
// their corrected date: they keep the camera's dates inside the file, and without an
// offload manifest line or a report entry their sidecars can't carry the new one.
// Scanning first gives them report entries; redate run again then records them.
func (r *run) noteUnrecorded() {
	if r.unrecordedSigned == 0 {
		return
	}
	verb := "had"
	if r.o.DryRun {
		verb = "would have"
	}
	r.warn("%d Content Credentials frame(s) %s nowhere to record the corrected date (no offload manifest line or report entry; their files keep the camera's dates): run `cull scan %s` first, then this redate again, so the report and their sidecars carry it",
		r.unrecordedSigned, verb, journal.ShellQuote(r.dir))
}

// noteSigned names the Content Credentials frames in one note: five, and the rest at
// verbose.
func (r *run) noteSigned() {
	s := r.res.Signed
	if len(s) == 0 {
		return
	}
	names := strings.Join(s, ", ")
	if len(s) > 5 {
		names = fmt.Sprintf("%s and %d more", strings.Join(s[:5], ", "), len(s)-5)
	}
	r.emit(ui.Note{Level: ui.Normal, Text: names +
		": Content Credentials — dates left unchanged so the signature stays valid (file times set; the sidecar carries the date)"})
	if len(s) > 5 {
		r.verbose("Content Credentials, dates left unchanged: %s", strings.Join(s, ", "))
	}
}

func (r *run) emit(n ui.Note) {
	if r.o.UI != nil {
		r.o.UI.Emit(ui.Event{Note: &n})
	}
}

// say is a dry run's per-file line: its whole output, shown even with -q.
func (r *run) say(format string, args ...any) {
	r.emit(ui.Note{Level: ui.Quiet, Text: fmt.Sprintf(format, args...)})
}

func (r *run) verbose(format string, args ...any) {
	r.emit(ui.Note{Level: ui.Verbose, Text: fmt.Sprintf(format, args...)})
}

func (r *run) warn(format string, args ...any) {
	r.emit(ui.Note{Sev: ui.Warn, Text: fmt.Sprintf(format, args...)})
}
