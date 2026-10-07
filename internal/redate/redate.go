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
	orphans                    int                                 // temps left alone that may be a file's only copy
	xattrNoted                 map[string]bool                     // extended-attribute failures noted, by kind
	warned                     bool                                // the catalogue warning was given
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
	if which, finish, ok := journal.Incomplete(dir); ok && which != "redate" {
		return r.res, fmt.Errorf("an unfinished %s is recorded in %s: finish it first with %s", which, dir, finish)
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
	folders, err := walkFolders(dir, o.Recursive)
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

	if !o.DryRun {
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
		case len(r.j.Files) > 0 || r.orphans > 0: // journalled and not finished, or a temp left alone
			r.warn("%d file(s) may be half done: run the same redate again to finish them", max(len(r.j.Files), r.orphans))
		default:
			r.traced("journal remove")
			if err := journal.RemoveRedate(dir); err != nil {
				runErr = err
			}
		}
		r.noteSigned()
	}
	return r.res, runErr
}

// file fixes one DNG. An error stops the run; a refusal is counted and returns nil.
func (r *run) file(p string) error {
	rel, _ := filepath.Rel(r.dir, p)
	st, err := os.Lstat(p)
	if err != nil {
		r.refuse(rel, err.Error())
		return nil
	}
	ps, skipped, err := patchesFor(p, st.Size(), r.o.Target)
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
	when := r.o.Target.Format(time.DateTime)
	if len(ps) == 0 && st.ModTime().Equal(r.o.Target) {
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

	pre := journal.FileState{Size: st.Size(), ModTime: st.ModTime()}
	if len(ps) == 0 {
		// Its bytes don't change, but a recorded file is still proven first: a damaged
		// frame is refused and left exactly as it is, as a patched one would be.
		if recorded {
			w, err := hex.DecodeString(e.CurrentSHA256())
			if err == nil && len(w) == 32 {
				err = offload.ProveFrom(r.ctx, p, [32]byte(w))
			}
			if err != nil {
				if r.ctx.Err() != nil {
					return r.ctx.Err()
				}
				r.refuse(rel, offload.ErrChanged.Error())
				return nil
			}
		}
		r.changing()
		r.j.Files[rel] = pre
		if err := r.saveJournal(); err != nil {
			return err
		}
		crErr, err := offload.SetFileTimes(p, r.o.Target)
		if err != nil { // the mtime didn't change: nothing to finish
			delete(r.j.Files, rel)
			r.refuse(rel, "its times can't be set: "+err.Error())
			return nil
		}
		r.crtime(crErr)
		r.res.TimesOnly++
		r.verbose("%s: file times → %s", rel, when)
		return r.record(p, rel, pre, folder, e, recorded, "")
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
	if err != nil && !rp.Swapped { // the original is untouched: nothing to finish
		delete(r.j.Files, rel)
	}
	switch {
	case journalErr != nil:
		return journalErr
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
	return r.record(p, rel, pre, folder, e, recorded, pre.Want)
}

// record does a changed file's bookkeeping: a superseding manifest line (want: the
// patched checksum, "" when only its times changed), its report entry, its sidecar.
func (r *run) record(p, rel string, pre journal.FileState, folder string, e offload.Entry, recorded bool, want string) error {
	now, err := os.Stat(p)
	if err != nil {
		r.refuse(rel, "after the change: "+err.Error())
		return nil
	}
	if recorded {
		if err := r.supersede(folder, e, want); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
	}
	r.follow(p, pre, now)
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
	needMan := recorded && (e.DatesSet != r.targetISO || (journaled && fs.Want != "" && e.FileSHA256 != fs.Want))
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
		w, err := hex.DecodeString(fs.Want)
		if err != nil || len(w) != 32 {
			r.refuse(rel, "the journal's checksum for it can't be read")
			return nil
		}
		if err := offload.ProveFrom(r.ctx, p, [32]byte(w)); err != nil {
			if r.ctx.Err() != nil {
				return r.ctx.Err()
			}
			r.refuse(rel, "its dates are set, but it isn't the file redate wrote: "+err.Error())
			return nil
		}
		want = fs.Want
	case needMan:
		w, err := hex.DecodeString(e.CurrentSHA256())
		if err != nil || len(w) != 32 {
			r.refuse(rel, "its manifest checksum can't be read")
			return nil
		}
		if err := offload.ProveFrom(r.ctx, p, [32]byte(w)); err != nil {
			if r.ctx.Err() != nil {
				return r.ctx.Err()
			}
			r.refuse(rel, offload.ErrChanged.Error())
			return nil
		}
	}
	if needMan {
		if err := r.supersede(folder, e, want); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
	}
	pre := journal.FileState{Size: st.Size(), ModTime: st.ModTime()}
	if follows {
		pre = fs
	}
	r.follow(p, pre, st)
	r.unsaved = append(r.unsaved, rel)
	return nil
}

// supersede appends e's superseding manifest line: the card's fields and checksum
// unchanged, plus the date set and, for a patched file, its checksum now.
func (r *run) supersede(folder string, e offload.Entry, want string) error {
	ne := e
	ne.DatesSet = r.targetISO
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
// The corrected date lives in DatesSet, which the sidecar uses.
func (r *run) follow(p string, pre journal.FileState, now os.FileInfo) {
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

func patchesFor(p string, size int64, t time.Time) ([]dng.Patch, []string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return dng.PatchDates(f, size, t)
}

// walkFolders lists dir, its sort folders (offload.MovedDirs), and with recursive
// every subfolder. Hidden folders are skipped. A subfolder holding its own report or
// offload manifest is another shoot folder, with its own bookkeeping: refused.
func walkFolders(dir string, recursive bool) ([]string, error) {
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
					return fmt.Errorf("%s is a shoot folder of its own (it has %s): run cull redate on each shoot folder, not -r on %s", p, own, dir)
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

// settleTemps deals with the redate temps a crash left in folder d. One beside its file
// is a swap that never happened: removed. One whose file is missing may be that file's
// only copy (a rename-over that deletes first, interrupted): if the journal records the
// file and the temp proves against its patched checksum, it takes the file's name;
// otherwise it is left alone and the journal stays. A dry run only says so.
func (r *run) settleTemps(d string) error {
	for tmp, target := range offload.RedateTemps(d) {
		rel, _ := filepath.Rel(r.dir, target)
		if _, err := os.Lstat(target); err == nil {
			if !r.o.DryRun {
				os.Remove(tmp)
			}
			continue
		}
		var rec journal.FileState
		ok := false
		if r.j != nil {
			rec, ok = r.j.Files[rel]
		}
		if r.o.DryRun || !ok || rec.Want == "" {
			r.orphans++
			r.warn("%s is missing, and %s may be its only copy: left alone (move it back to %s if it is the frame)", rel, filepath.Base(tmp), filepath.Base(target))
			continue
		}
		w, err := hex.DecodeString(rec.Want)
		if err == nil && len(w) == 32 {
			err = offload.ProveFrom(r.ctx, tmp, [32]byte(w))
		}
		if err != nil {
			if r.ctx.Err() != nil {
				return r.ctx.Err()
			}
			r.orphans++
			r.warn("%s is missing, and %s doesn't prove against what redate wrote (%v): left alone", rel, filepath.Base(tmp), err)
			continue
		}
		if err := offload.Adopt(tmp, target); err != nil {
			return fmt.Errorf("restore %s from %s: %w", rel, filepath.Base(tmp), err)
		}
		r.warn("%s: restored from its proven temp (an interrupted swap)", rel)
	}
	return nil
}

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
