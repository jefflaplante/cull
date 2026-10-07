// Package rename renames a shoot folder's frames by a pattern (cull rename), and puts
// the last rename back (--undo). It moves no bytes, but every record follows the files:
// the report's keys (so the next judge re-bills nothing), the labels log, the offload
// manifest, cull's sidecars and the review sheet's cached images.
//
// A rename is two phases, journalled (cull-rename.json) and flushed to the media
// before any file moves: every frame (with its sidecar) to a hidden temp beside it,
// then every temp to its new name, so frames can swap names. Nothing is ever replaced
// (a hard link refuses an existing name; where there are none, a check, then
// rename(2)), and nothing is deleted. A crash leaves the journal unfinished: running
// the same command again finishes each move from wherever its file is (old name, temp
// or new name), and anything it can't be sure of is left as it is and reported, with
// what to do.
package rename

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jefflaplante/cull/internal/dcf"
	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/review"
	"github.com/jefflaplante/cull/internal/ui"
	"github.com/jefflaplante/cull/internal/xmp"
	"golang.org/x/sys/unix"
)

// Options are one rename run.
type Options struct {
	Dir, ReportPath, Pattern string // ReportPath "" = <Dir>/cull-report.json
	Recursive                bool   // subfolders too (each must not be a shoot folder of its own)
	DryRun, Undo             bool
	Reorder                  bool // go ahead when the new names change the frames' order (sets regroup)
	UI                       ui.Sink
}

// Move is one frame's move, by absolute paths; its sidecar moves alongside when it
// has one.
type Move struct{ Old, Tmp, New string }

// Result is what a run did (with DryRun: would do).
type Result struct {
	Renamed int    // frames under their new names
	Moves   []Move // the plan (or the unfinished run's)
	// Held are moves left as they are because the run couldn't be sure where the frame
	// is: "<path>: <what is there>: <what to do>". The journal stays unfinished.
	Held []string
}

// crash is a test seam: true stops Run right after that step ("phase1"/"phase2" after
// move i; "report1" after the report's first save, "report" once the journal records
// it, "report2" after its second save; "labels", "manifest"), as a crash would.
var crash func(step string, i int) bool

var errCrash = errors.New("rename: stopped (test)")

// trace is a test seam: the run's moves and durability steps, in order.
var trace func(event string)

type run struct {
	o                            Options
	dir, reportPath, reportField string
	rep                          *report.Report
	labPath                      string
	lab                          map[string]labels.Entry
	j                            *journal.Rename
	res                          Result
}

const catalogueWarning = "if these frames are already in a Capture One or Lightroom catalogue, it may lose track of the changed files"

// Run renames every DNG in o.Dir and its keep/, review/, cull/ and culled/ folders
// (subfolders with Recursive) by o.Pattern, in camera order; with Undo it puts back
// the names the last rename changed. Each frame keeps its folder.
func Run(ctx context.Context, o Options) (Result, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return Result{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Result{}, fmt.Errorf("%s is not a directory", o.Dir)
	}
	r := &run{o: o, dir: dir}
	defReport := filepath.Join(dir, "cull-report.json")
	r.reportPath = cmp.Or(o.ReportPath, defReport)
	if r.reportPath != defReport {
		r.reportField = r.reportPath
	}
	if slices.Contains(offload.MovedDirs, filepath.Base(dir)) {
		return r.res, fmt.Errorf("%s is a sort folder: run cull rename on its shoot folder, %s (its sorted frames are renamed where they are)", dir, filepath.Dir(dir))
	}
	if !o.DryRun {
		release, note, err := journal.Lock(dir, true, "rename")
		if err != nil {
			return r.res, err
		}
		defer release()
		if note != "" {
			r.warn("%s", note)
		}
	}
	pending := journal.Unfinished(dir)
	if o.Recursive {
		pending = journal.IncompleteBelow(dir)
	}
	for _, p := range pending {
		if p.Which != "rename" || p.Folder != dir {
			return r.res, fmt.Errorf("an unfinished %s is recorded in %s: finish it first with %s", p.Which, p.Folder, p.Finish)
		}
	}
	if err := journal.PendingBatch(r.reportPath); err != nil {
		return r.res, err
	}
	j, err := journal.LoadRename(dir)
	if err != nil {
		return r.res, fmt.Errorf("%v: frames may be under hidden .cull-rename- temps that only it records; cull status %s lists them; restore the journal from a backup rather than delete it", err, journal.ShellQuote(dir))
	}
	r.otherReports()
	unfinished := j != nil && !j.Complete
	if !o.Undo && !unfinished {
		if err := offload.ValidatePattern(o.Pattern); err != nil {
			return r.res, fmt.Errorf("pattern %w", err)
		}
	}
	switch {
	case o.Undo && j == nil:
		return r.res, fmt.Errorf("no rename is recorded in %s (%s): nothing to undo", dir, journal.RenameName)
	case o.Undo && j.Undo && j.Complete:
		return r.res, fmt.Errorf("the last rename in %s was already undone", dir)
	case !o.Undo && unfinished && j.Undo:
		return r.res, fmt.Errorf("an unfinished rename --undo is recorded in %s: finish it first with %s", dir, j.Finish(dir))
	case !o.Undo && unfinished && j.Pattern != o.Pattern:
		return r.res, fmt.Errorf("an unfinished rename to %s is recorded in %s: finish it first with %s", journal.ShellQuote(j.Pattern), dir, j.Finish(dir))
	}
	if (unfinished || o.Undo) && (j.Recursive != o.Recursive || j.Report != r.reportField) {
		u := *j
		if o.Undo {
			u.Undo = true
		}
		return r.res, fmt.Errorf("the rename recorded in %s ran with recursive %v and report %s: run %s",
			filepath.Join(dir, journal.RenameName), j.Recursive, cmp.Or(j.Report, defReport), u.Finish(dir))
	}

	// Everything the run updates is read first: no file moves while its records can't
	// follow.
	switch rep, err := report.Load(r.reportPath); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return r.res, fmt.Errorf("%s can't be read (%v): its keys must follow the files, so nothing was renamed", r.reportPath, err)
	default:
		rep.Relocate(r.reportPath, dir)
		r.rep = rep
	}
	r.labPath = labels.DefaultPath(r.reportPath)
	if r.lab, err = labels.Read(r.labPath); err != nil {
		return r.res, fmt.Errorf("%v: labels must follow the files, so nothing was renamed", err)
	}

	var next *journal.Rename
	switch {
	case unfinished && j.Undo == o.Undo:
		next = j
	case o.Undo && unfinished:
		next = r.undoUnfinished(j)
	case o.Undo:
		next, err = r.undoPlan(j)
	default:
		next, err = r.plan()
	}
	if err != nil {
		return r.res, err
	}
	if next == nil {
		r.say("nothing to rename: every frame already has its name")
		return r.res, nil
	}
	for _, m := range next.Moves {
		r.res.Moves = append(r.res.Moves, Move{Old: r.abs(m.Old), Tmp: r.abs(m.Tmp), New: r.abs(m.New)})
	}
	if o.DryRun {
		if unfinished {
			r.say("an unfinished rename is recorded in %s: a run finishes it, moving each frame on from wherever it is", dir)
		}
		for _, m := range next.Moves {
			r.say("%s → %s", m.Old, m.New)
		}
		return r.res, nil
	}
	if err := ctx.Err(); err != nil { // nothing has changed yet
		return r.res, err
	}
	r.j = next
	return r.res, r.execute()
}

func (r *run) abs(rel string) string { return filepath.Join(r.dir, rel) }

func (r *run) rel(p string) string {
	rel, err := filepath.Rel(r.dir, p)
	if err != nil {
		return p
	}
	return rel
}

// frame is a DNG found in the folders.
type frame struct {
	rel, unit  string // unit: the shoot folder whose manifest records it (absolute)
	size       int64
	mtime      time.Time
	orig       string // the manifest's camera name; "" unrecorded
	card       string // with orig, its card folder and name ("100LEICA/M1103127.DNG"): camera order
	origSize   int64  // with orig, the manifest's key for it
	datesSet   string // the manifest's date set, "2026-10-04T12:00:00"
	hasSidecar bool
	locked     string // "" or why it can't be renamed (an immutable flag)
}

func (f frame) base() string { return filepath.Base(f.rel) }

// home is the absolute path the report gives the frame at rel: in a sort folder,
// its shoot folder's path for it (as triples).
func (r *run) home(rel string) string {
	p := r.abs(rel)
	return filepath.Join(redate.ShootOf(r.dir, filepath.Dir(p)), filepath.Base(p))
}

// shoot is what survey found: the frames, every name in the shoot's folders, and the
// names its manifest records for frames no longer there.
type shoot struct {
	frames []frame
	names  map[string]map[string][]string // shoot folder → lower-case name → relative paths
	ghosts map[string]map[string]string   // shoot folder → lower-case name → manifest name, its frame missing
	dirs   map[string][]string            // folder (relative) → names in it
}

// survey lists the frames in folders, and every name in them, for the checks.
func (r *run) survey(folders []string) (*shoot, error) {
	man := map[string]map[string]offload.Entry{}
	sh := &shoot{names: map[string]map[string][]string{}, ghosts: map[string]map[string]string{}, dirs: map[string][]string{}}
	for _, d := range folders {
		unit := redate.ShootOf(r.dir, d)
		if _, ok := man[unit]; !ok {
			es, err := offload.CurrentManifest(unit)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Join(unit, offload.ManifestName), err)
			}
			m := map[string]offload.Entry{}
			for _, e := range es {
				m[strings.ToLower(e.Name)] = e
			}
			man[unit], sh.names[unit] = m, map[string][]string{}
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			rel := r.rel(filepath.Join(d, e.Name()))
			low := strings.ToLower(e.Name())
			sh.names[unit][low] = append(sh.names[unit][low], rel)
			sh.dirs[r.rel(d)] = append(sh.dirs[r.rel(d)], e.Name())
			if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !strings.EqualFold(filepath.Ext(e.Name()), ".dng") {
				continue
			}
			st, err := os.Lstat(filepath.Join(d, e.Name()))
			if err != nil {
				return nil, err
			}
			f := frame{rel: rel, unit: unit, size: st.Size(), mtime: st.ModTime(), locked: lockedFlag(st)}
			if me, ok := man[unit][low]; ok {
				f.orig, f.origSize, f.datesSet, f.card = me.Orig, me.Size, me.DatesSet, me.CardName()
			}
			if st, err := os.Lstat(xmp.Path(r.abs(rel))); err == nil && st.Mode().IsRegular() {
				f.hasSidecar = true
				if why := lockedFlag(st); why != "" && f.locked == "" {
					f.locked = "its sidecar " + why
				}
			}
			sh.frames = append(sh.frames, f)
		}
	}
	// Names must be unique in a shoot (labels and the report's home paths rely on it).
	seen := map[string]string{}
	for _, f := range sh.frames {
		k := f.unit + "\x00" + strings.ToLower(f.base())
		if other, ok := seen[k]; ok {
			return nil, fmt.Errorf("%s and %s have the same name in one shoot: rename relies on unique names; move one aside", other, f.rel)
		}
		seen[k] = f.rel
	}
	// A manifest name whose frame is gone (deleted, say) stays taken: an offload re-run
	// or --verify would take a new frame of that name for it.
	for unit, m := range man {
		sh.ghosts[unit] = map[string]string{}
		for low, e := range m {
			if _, ok := seen[unit+"\x00"+low]; !ok {
				sh.ghosts[unit][low] = e.Name
			}
		}
	}
	return sh, nil
}

// refuseOrphans refuses a new rename while hidden rename temps no unfinished rename
// records are in the folders: one may be a frame's only copy.
func (r *run) refuseOrphans(folders []string) error {
	var found []string
	for _, d := range folders {
		for tmp := range offload.RenameTemps(d) {
			found = append(found, tmp)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("hidden rename temps that no unfinished rename records are in the shoot: %s; one may be a frame's only copy: cull status %s says what to do with each; nothing was renamed",
		strings.Join(found, ", "), journal.ShellQuote(r.dir))
}

var datedFolder = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?: (.+))?$`)

// plan names every frame by the pattern, in camera order (dcf: the DCF folder and
// file counter, so an M11-P's M… and L… frames, numbered from one counter, interleave
// as shot) of the manifest's card name where it records one, else of the current
// name. nil: nothing changes.
func (r *run) plan() (*journal.Rename, error) {
	folders, err := redate.Folders(r.dir, r.o.Recursive, "rename")
	if err != nil {
		return nil, err
	}
	if err := r.refuseOrphans(folders); err != nil {
		return nil, err
	}
	sh, err := r.survey(folders)
	if err != nil {
		return nil, err
	}
	frames := sh.frames
	cam := make([]dcf.Name, len(frames))
	for i, f := range frames {
		cam[i] = dcf.Of(cmp.Or(f.card, f.base()))
	}
	dcf.Unify(cam)
	idx := make([]int, len(frames))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if c := dcf.Compare(cam[ia], cam[ib]); c != 0 {
			return c < 0
		}
		return frames[ia].rel < frames[ib].rel
	})
	frames = make([]frame, len(idx))
	for k, i := range idx {
		frames[k] = sh.frames[i]
	}
	folderDate, name := "", filepath.Base(r.dir)
	if m := datedFolder.FindStringSubmatch(name); m != nil {
		folderDate, name = m[1]+m[2]+m[3], m[4]
	}
	newBase := map[string]string{}
	for i, f := range frames {
		date := ""
		if strings.Contains(r.o.Pattern, "{date}") {
			if date = r.captureDate(f); date == "" {
				date = folderDate
			}
			if date == "" {
				return nil, fmt.Errorf("%s: no capture date can be read, and the folder's name doesn't start with one: {date} can't be filled", f.rel)
			}
		}
		orig := cmp.Or(f.orig, f.base())
		stem, err := offload.ExpandName(r.o.Pattern, date, name, strings.TrimSuffix(orig, filepath.Ext(orig)), i+1)
		if err != nil {
			return nil, fmt.Errorf("pattern %w", err)
		}
		newBase[f.rel] = stem + strings.ToUpper(filepath.Ext(f.base()))
	}
	moves, err := r.check(sh, newBase)
	if err != nil || len(moves) == 0 {
		return nil, err
	}
	cards, err := r.checkOrder(sh, moves)
	if err != nil {
		return nil, err
	}
	return &journal.Rename{Pattern: r.o.Pattern, Recursive: r.o.Recursive, Report: r.reportField, Started: time.Now(), Moves: moves, Phase: 1, Cards: cards}, nil
}

// checkOrder refuses a rename that changes the order of the report's frames as
// sequence grouping sees it (group.Order: capture time, then camera order of the
// report's card name, else the current name; frames with the same capture time, as a
// burst or a stopped clock gives, are ordered by name only when no manifest records
// them). A new order regroups
// the shoot's sets, and judge would rank the changed sets again. Reorder goes ahead,
// saying how many sets change.
//
// A report written before card names were recorded lacks them; the next judge fills
// them from the manifest, so both orders here take them from the manifest too (the
// frames' card names), and the rename writes them into the report (the returned map,
// by the report's path relative to the folder; journalled, see followReport).
func (r *run) checkOrder(sh *shoot, moves []journal.RenameMove) (map[string]string, error) {
	if r.rep == nil {
		return nil, nil
	}
	cardOf := map[string]string{} // the report's (home) path → the manifest's card name
	for _, f := range sh.frames {
		if f.card != "" {
			cardOf[r.home(f.rel)] = f.card
		}
	}
	cards := map[string]string{}
	to := map[string]string{} // the report's (home) path → the new one
	for _, m := range moves {
		t := r.triples(m)
		h := t[0]
		if len(t) > 2 {
			h = t[2]
		}
		to[h[0]] = h[2]
	}
	var before, after []group.Frame
	var dcfNamed string // a new name that reads as a camera file number
	for _, m := range moves {
		if dcf.IsName(filepath.Base(m.New)) && dcfNamed == "" {
			dcfNamed = filepath.Base(m.New)
		}
	}
	for _, x := range r.rep.Results {
		if c, ok := cardOf[x.File]; ok && x.CardName == "" {
			x.CardName, cards[r.rel(x.File)] = c, c
		}
		if x.Error != "" || x.Preview == nil {
			continue
		}
		before = append(before, x.GroupFrame())
		if n, ok := to[x.File]; ok {
			x.File = n // what the renamed report gives decide and judge
		}
		after = append(after, x.GroupFrame())
	}
	if len(cards) == 0 {
		cards = nil
	}
	if slices.Equal(group.Order(before), group.Order(after)) {
		return cards, nil
	}
	changed := -1
	if r.rep.Seq != nil && r.rep.Seq.GapSeconds > 0 {
		changed = setsChanged(group.Sequences(before, r.rep.Seq.Options()), group.Sequences(after, r.rep.Seq.Options()))
	}
	if r.o.Reorder {
		if changed >= 0 {
			r.warn("the new names put frames with the same capture time in another order: %d set(s) change, and judge ranks them again", changed)
		} else {
			r.warn("the new names put frames with the same capture time in another order")
		}
		return cards, nil
	}
	cause := "frames no offload manifest records are ordered by their names, and the new names sort frames with the same capture time in another order than their names do now"
	switch {
	case dcfNamed != "":
		cause = fmt.Sprintf("new names of 8 characters ending in 4 digits, such as %s, read as camera file numbers and are ordered by those 4 digits: pick a pattern that gives names of another length", dcfNamed)
	case strings.Contains(r.o.Pattern, "{n}") && len(before) >= 10:
		cause = fmt.Sprintf("{n} is unpadded, so 10 sorts before 2: use {n:%d}", max(4, len(strconv.Itoa(len(before)))))
	}
	sets := ""
	if changed >= 0 {
		sets = fmt.Sprintf(" (%d set(s) would change)", changed)
	}
	return nil, fmt.Errorf("this rename would change the order of the report's frames, which regroups the shoot's sets%s and makes judge rank them again: %s; or pass --reorder to go ahead; nothing was renamed", sets, cause)
}

// setsChanged counts the sets in a that b doesn't have, and those in b that a doesn't.
func setsChanged(a, b [][]int) int {
	key := func(s []int) string {
		c := slices.Clone(s)
		slices.Sort(c)
		return fmt.Sprint(c)
	}
	in := func(sets [][]int) map[string]bool {
		m := map[string]bool{}
		for _, s := range sets {
			m[key(s)] = true
		}
		return m
	}
	ia, ib := in(a), in(b)
	n := 0
	for k := range ia {
		if !ib[k] {
			n++
		}
	}
	for k := range ib {
		if !ia[k] {
			n++
		}
	}
	return n
}

// captureDate is f's capture date as YYYYMMDD: the date offload --set-date or redate
// set (the manifest records it), else EXIF DateTimeOriginal; "" when neither reads.
func (r *run) captureDate(f frame) string {
	if d := strings.ReplaceAll(f.datesSet, "-", ""); len(d) >= 8 && allDigits(d[:8]) {
		return d[:8]
	}
	ex, err := dng.ReadExif(r.abs(f.rel))
	if err != nil {
		return ""
	}
	if d := strings.ReplaceAll(ex.DateTimeOriginal, ":", ""); len(d) >= 8 && allDigits(d[:8]) && d[:8] != "00000000" {
		return d[:8]
	}
	return ""
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// check turns the new names (by frame, relative path → base name) into moves. It
// refuses, before anything changes and listing every problem, when:
//   - two frames of a shoot would share a name;
//   - a new name, its temp's or its sidecar's, can't be a file name here (over 255
//     bytes, "/", ":", NUL, a leading ".");
//   - a frame or its sidecar is locked, or a folder can't be written;
//   - a moving frame has companions that wouldn't follow it (a camera JPG, a
//     "<name>.xmp", Capture One's settings): only "<stem>.xmp" moves with it;
//   - any file in the shoot (or a manifest name whose frame is gone) holds a new stem.
func (r *run) check(sh *shoot, newBase map[string]string) ([]journal.RenameMove, error) {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	final := map[string]string{} // unit + lower-case final name → frame
	vacate := map[string]bool{}  // lower-case relative paths moving away
	isFrame := map[string]bool{} // lower-case relative paths of frames
	for _, f := range sh.frames {
		isFrame[strings.ToLower(f.rel)] = true
		nb := cmp.Or(newBase[f.rel], f.base())
		k := f.unit + "\x00" + strings.ToLower(nb)
		if other, ok := final[k]; ok {
			bad("%s and %s would both be named %s", other, f.rel, nb)
		}
		final[k] = f.rel
		if nb != f.base() {
			vacate[strings.ToLower(f.rel)] = true
			if f.hasSidecar {
				vacate[strings.ToLower(r.rel(xmp.Path(r.abs(f.rel))))] = true
			}
		}
	}
	var moves []journal.RenameMove
	folders := map[string]bool{}
	for _, f := range sh.frames {
		nb := cmp.Or(newBase[f.rel], f.base())
		if nb == f.base() {
			continue
		}
		d := filepath.Dir(f.rel)
		folders[d] = true
		m := journal.RenameMove{Old: f.rel, Tmp: filepath.Join(d, offload.RenameTempName(f.base())), New: filepath.Join(d, nb),
			Size: f.size, ModTime: f.mtime, Orig: f.orig, OrigSize: f.origSize}
		newStem := strings.TrimSuffix(nb, filepath.Ext(nb))
		for _, n := range []struct{ what, name string }{
			{"the new name", nb}, {"its hidden temp", filepath.Base(m.Tmp)},
			{"its sidecar's new name", newStem + ".xmp"}, {"its sidecar's hidden temp", filepath.Base(xmp.Path(m.Tmp))},
		} {
			if why := nameProblem(n.name, strings.Contains(n.what, "temp")); why != "" {
				bad("%s: %s %q %s", f.rel, n.what, n.name, why)
			}
		}
		if f.locked != "" {
			bad("%s: %s: unlock it (Finder: Get Info, Locked) or leave it out", f.rel, f.locked)
		}
		// Companions that wouldn't follow it.
		stem := strings.ToLower(strings.TrimSuffix(f.base(), filepath.Ext(f.base())))
		for _, n := range sh.dirs[d] {
			low := strings.ToLower(n)
			if strings.HasPrefix(low, stem+".") && !strings.HasPrefix(n, "._") && low != strings.ToLower(f.base()) &&
				low != stem+".xmp" && !isFrame[strings.ToLower(filepath.Join(d, n))] {
				bad("%s: %s would keep the old name (rename moves only the frame and its .xmp sidecar): move it aside, or rename it with its frame yourself", f.rel, filepath.Join(d, n))
			}
		}
		if c1 := captureOneSettings(r.abs(d), f.base()); c1 != "" {
			bad("%s: Capture One's settings %s would keep the old name and lose the frame's edits: rename it in Capture One instead, or move the settings aside", f.rel, r.rel(c1))
		}
		// Anything in the shoot named for the new stem.
		for low, ats := range sh.names[f.unit] {
			if !strings.HasPrefix(low, strings.ToLower(newStem)+".") {
				continue
			}
			for _, at := range ats {
				if !vacate[strings.ToLower(at)] && !strings.HasPrefix(filepath.Base(at), "._") && strings.ToLower(at) != strings.ToLower(f.rel) {
					bad("%s: %s already exists, with the stem of its new name %s: move it aside or pick another pattern", f.rel, at, nb)
				}
			}
		}
		if g, ok := sh.ghosts[f.unit][strings.ToLower(nb)]; ok {
			bad("%s: its new name %s is the name the offload manifest records for a frame no longer in the shoot (deleted?): pick another pattern", f.rel, g)
		}
		if f.hasSidecar {
			m.SidecarOld, m.SidecarTmp, m.SidecarNew = r.rel(xmp.Path(r.abs(m.Old))), r.rel(xmp.Path(r.abs(m.Tmp))), r.rel(xmp.Path(r.abs(m.New)))
		}
		if e, ok := r.lab[f.base()]; ok {
			e := e
			m.Label = &e
		}
		moves = append(moves, m)
	}
	for d := range folders {
		if err := unix.Access(r.abs(d), unix.W_OK); err != nil {
			bad("%s can't be written (%v)", r.abs(d), err)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("nothing was renamed:\n  %s", strings.Join(problems, "\n  "))
	}
	sort.Slice(moves, func(a, b int) bool { return moves[a].Old < moves[b].Old })
	return moves, nil
}

// nameProblem says why name can't be a file name here ("" if it can); a frame's or a
// sidecar's name must not be hidden either (a temp's is, on purpose).
func nameProblem(name string, temp bool) string {
	switch {
	case len(name) > 255:
		return fmt.Sprintf("is %d bytes; a file name holds at most 255", len(name))
	case strings.ContainsAny(name, "/:\x00"):
		return `holds "/", ":" or NUL`
	case !temp && strings.HasPrefix(name, "."):
		return "starts with \".\": hidden, never taken for a frame"
	}
	return ""
}

// captureOneSettings is Capture One's settings file for the frame name in folder d
// (CaptureOne/Settings*/<name>.cos), or "".
func captureOneSettings(d, name string) string {
	ents, _ := os.ReadDir(filepath.Join(d, "CaptureOne"))
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "Settings") {
			continue
		}
		p := filepath.Join(d, "CaptureOne", e.Name(), name+".cos")
		if _, err := os.Lstat(p); err == nil {
			return p
		}
	}
	return ""
}

// undoPlan reverses a completed rename: each frame it named goes back to its name
// before, in the folder it is in now (sorting may have moved it since).
func (r *run) undoPlan(fwd *journal.Rename) (*journal.Rename, error) {
	folders, err := redate.Folders(r.dir, fwd.Recursive, "rename")
	if err != nil {
		return nil, err
	}
	if err := r.refuseOrphans(folders); err != nil {
		return nil, err
	}
	sh, err := r.survey(folders)
	if err != nil {
		return nil, err
	}
	at := map[string]frame{} // unit + lower-case name → frame
	for _, f := range sh.frames {
		at[f.unit+"\x00"+strings.ToLower(f.base())] = f
	}
	newBase := map[string]string{}
	for _, m := range fwd.Moves {
		unit := redate.ShootOf(r.dir, filepath.Dir(r.abs(m.New)))
		f, ok := at[unit+"\x00"+strings.ToLower(filepath.Base(m.New))]
		if !ok {
			return nil, fmt.Errorf("%s isn't in the shoot any more (moved or deleted since the rename?): nothing was put back", m.New)
		}
		newBase[f.rel] = filepath.Base(m.Old)
	}
	moves, err := r.check(sh, newBase)
	if err != nil || len(moves) == 0 {
		return nil, err
	}
	return &journal.Rename{Pattern: fwd.Pattern, Undo: true, Recursive: fwd.Recursive, Report: fwd.Report, Started: time.Now(), Moves: moves, Phase: 1}, nil
}

// undoUnfinished reverses a rename that stopped part way: each frame goes back from
// wherever it is (its old name, its temp, its new name) to its old name, through the
// same temp. Stopped in phase 1, no new name was taken yet: every frame is at its old
// name or its temp, so the undo starts with phase 2 (temp → old name).
func (r *run) undoUnfinished(fwd *journal.Rename) *journal.Rename {
	u := &journal.Rename{Pattern: fwd.Pattern, Undo: true, Recursive: fwd.Recursive, Report: fwd.Report, Started: time.Now(), Phase: 1}
	for _, m := range fwd.Moves {
		um := journal.RenameMove{Old: m.New, Tmp: m.Tmp, New: m.Old, SidecarOld: m.SidecarNew, SidecarTmp: m.SidecarTmp, SidecarNew: m.SidecarOld,
			Size: m.Size, ModTime: m.ModTime, Orig: m.Orig, OrigSize: m.OrigSize, Label: m.Label}
		if fwd.Phase <= 1 {
			// Its old name or its temp holds it (nothing else can, in phase 1): what is
			// there is the frame, as it is now.
			for _, p := range []string{m.Tmp, m.Old} {
				if st, err := os.Lstat(r.abs(p)); err == nil && st.Mode().IsRegular() {
					um.Size, um.ModTime = st.Size(), st.ModTime()
					break
				}
			}
		}
		u.Moves = append(u.Moves, um)
	}
	if fwd.Phase <= 1 {
		u.Phase = 2
	}
	switch r.reportAt(fwd) {
	case "":
		u.ReportAt = "new"
	case "temp":
		u.ReportAt = "temp"
	}
	return u
}

// execute moves the files in two phases, then makes every record follow.
func (r *run) execute() error {
	r.warn(catalogueWarning)
	if err := r.saveJournal(); err != nil {
		return err
	}
	if r.j.Phase <= 1 {
		for i := range r.j.Moves {
			r.toTemp(&r.j.Moves[i])
			if crash != nil && crash("phase1", i) {
				return errCrash
			}
		}
		if err := r.flushFolders(); err != nil {
			return err
		}
		if len(r.res.Held) > 0 {
			return r.stopHeld()
		}
		r.j.Phase = 2 // with each frame's size and time as its temp has them
		if err := r.saveJournal(); err != nil {
			return err
		}
	}
	for i := range r.j.Moves {
		if r.toNew(&r.j.Moves[i]) {
			r.res.Renamed++
		}
		if crash != nil && crash("phase2", i) {
			return errCrash
		}
	}
	if err := r.flushFolders(); err != nil {
		return err
	}
	if len(r.res.Held) > 0 {
		return r.stopHeld()
	}
	if err := r.followReport(); err != nil {
		return err
	}
	if err := r.followLabels(); err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	if crash != nil && crash("labels", 0) {
		return errCrash
	}
	if err := r.followManifests(); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if crash != nil && crash("manifest", 0) {
		return errCrash
	}
	r.followCache()
	if r.j.Undo {
		if err := journal.RemoveRename(r.dir); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		r.traced("journal remove")
		return r.flush(r.dir)
	}
	r.j.Complete = true
	return r.saveJournal()
}

// toTemp is phase 1 for one move: the frame, then its sidecar, to their temps.
func (r *run) toTemp(m *journal.RenameMove) {
	old, tmp := r.abs(m.Old), r.abs(m.Tmp)
	oldSt, oldErr := os.Lstat(old)
	tmpSt, tmpErr := os.Lstat(tmp)
	switch {
	case tmpErr == nil && oldErr == nil:
		if !os.SameFile(oldSt, tmpSt) {
			r.hold(old, "it and its hidden temp %s are both there and aren't one file: compare them and move aside the one that isn't the frame (on exFAT or FAT, run First Aid on the volume first: after a crash both names can share one file's data), then run %s", q(tmp), r.finish())
			return
		}
		if err := os.Remove(old); err != nil { // a link made just before a crash: the frame stays as its temp
			r.hold(old, "it and its hidden temp %s are one file (a link), and the old name can't be removed (%v): fix that, then run %s", q(tmp), err, r.finish())
			return
		}
	case tmpErr == nil:
	case oldErr == nil:
		if err := r.move(old, tmp); err != nil {
			r.hold(old, "can't move it to its hidden temp %s (%v): fix that, then run %s", q(tmp), err, r.finish())
			return
		}
	default:
		r.hold(old, "neither it nor its hidden temp %s is there: if you moved the frame, put it back as %s, then run %s", q(tmp), q(old), r.finish())
		return
	}
	// The temp holds the frame now: phase 2 checks against it as it is.
	if st, err := os.Lstat(tmp); err == nil && (st.Size() != m.Size || !st.ModTime().Equal(m.ModTime)) {
		r.warn("%s changed since the rename began (size or modification time); it is renamed as it is", m.Old)
		m.Size, m.ModTime = st.Size(), st.ModTime()
	}
	if m.SidecarOld == "" {
		return
	}
	sOld, sTmp := r.abs(m.SidecarOld), r.abs(m.SidecarTmp)
	switch _, oErr, _, tErr := lstat2(sOld, sTmp); {
	case tErr == nil && oErr == nil:
		if same(sOld, sTmp) {
			os.Remove(sOld)
			return
		}
		r.hold(sOld, "it and its hidden temp %s are both there and aren't one file: move aside the one that isn't the frame's sidecar, then run %s", q(sTmp), r.finish())
	case tErr == nil:
	case oErr == nil:
		if err := r.move(sOld, sTmp); err != nil {
			r.hold(sOld, "can't move it to its hidden temp %s (%v): fix that, then run %s", q(sTmp), err, r.finish())
		}
	default:
		r.warn("%s: its sidecar %s is gone; the frame is renamed without it", m.Old, m.SidecarOld)
	}
}

// toNew is phase 2 for one move: the temps to the new names. Every old name is free by
// now (phase 1 finished), so a file at the new name is this frame or a stranger's. It
// reports whether the frame is under its new name.
func (r *run) toNew(m *journal.RenameMove) bool {
	tmp, nw := r.abs(m.Tmp), r.abs(m.New)
	tmpSt, tmpErr := os.Lstat(tmp)
	newSt, newErr := os.Lstat(nw)
	switch {
	case tmpErr == nil && newErr == nil:
		if !os.SameFile(tmpSt, newSt) {
			r.hold(nw, "something else is there, and the frame is in its hidden temp %s: move %s aside, then run %s", q(tmp), q(nw), r.finish())
			return false
		}
		if err := os.Remove(tmp); err != nil {
			r.hold(nw, "it and the hidden temp %s are one file (a link), and the temp's name can't be removed (%v): fix that, then run %s", q(tmp), err, r.finish())
			return false
		}
	case tmpErr == nil:
		if err := r.move(tmp, nw); err != nil {
			r.hold(nw, "can't move the frame there from its hidden temp %s (%v): fix that, then run %s", q(tmp), err, r.finish())
			return false
		}
	case newErr == nil:
		if !newSt.Mode().IsRegular() || newSt.Size() != m.Size || !newSt.ModTime().Equal(m.ModTime) {
			r.hold(nw, "the file there isn't the one the rename recorded (size or modification time), and the frame's hidden temp %s is gone: find the frame (cull status lists hidden temps), put it at %s, then run %s", q(tmp), q(nw), r.finish())
			return false
		}
	default:
		r.hold(nw, "the frame is neither there nor in its hidden temp %s: find it (cull status lists hidden temps), put it at %s, then run %s", q(tmp), q(nw), r.finish())
		return false
	}
	if m.SidecarOld == "" {
		return true
	}
	sTmp, sNew := r.abs(m.SidecarTmp), r.abs(m.SidecarNew)
	switch _, tErr, _, nErr := lstat2(sTmp, sNew); {
	case tErr == nil && nErr == nil:
		if same(sTmp, sNew) {
			os.Remove(sTmp)
			return true
		}
		r.hold(sNew, "something else is there, and the frame's sidecar is in its hidden temp %s: move %s aside, then run %s", q(sTmp), q(sNew), r.finish())
	case tErr == nil:
		if err := r.move(sTmp, sNew); err != nil {
			r.hold(sNew, "can't move the sidecar there from its hidden temp %s (%v): fix that, then run %s", q(sTmp), err, r.finish())
		}
	case nErr == nil:
	default:
		r.warn("%s: its sidecar is gone; the frame is renamed without it", m.New)
	}
	return true
}

func lstat2(a, b string) (os.FileInfo, error, os.FileInfo, error) {
	sa, ea := os.Lstat(a)
	sb, eb := os.Lstat(b)
	return sa, ea, sb, eb
}

func same(a, b string) bool {
	sa, ea := os.Lstat(a)
	sb, eb := os.Lstat(b)
	return ea == nil && eb == nil && os.SameFile(sa, sb)
}

func q(p string) string { return journal.ShellQuote(p) }

// finish is the command that finishes (or, for a rename, reverses) the run.
func (r *run) finish() string { return r.j.Finish(r.dir) }

func (r *run) hold(p, format string, args ...any) {
	msg := q(p) + ": " + fmt.Sprintf(format, args...)
	r.res.Held = append(r.res.Held, msg)
	r.warn("%s", msg)
}

// stopHeld ends a run with held moves: the journal stays unfinished (judge, decide,
// review, restore and redate refuse meanwhile).
func (r *run) stopHeld() error {
	return fmt.Errorf("%d move(s) left as they are (see above); the rename is unfinished: once they're dealt with, run %s", len(r.res.Held), r.finish())
}

// folders are the folders the moves are in.
func (r *run) folders() []string {
	set := map[string]bool{}
	for _, m := range r.j.Moves {
		set[filepath.Dir(r.abs(m.Old))] = true
	}
	var out []string
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// flushFolders carries the folders' entries (the moves just made) to the media.
func (r *run) flushFolders() error {
	for _, d := range r.folders() {
		if err := r.flush(d); err != nil {
			return fmt.Errorf("flush %s: %w", d, err)
		}
	}
	return nil
}

// saveJournal writes the journal and carries it to the media (F_FULLFSYNC; fsync on a
// network share) before anything that depends on it.
func (r *run) saveJournal() error {
	if err := r.j.Save(r.dir); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	r.traced(fmt.Sprintf("journal phase=%d report=%s complete=%v", r.j.Phase, r.j.ReportAt, r.j.Complete))
	for _, p := range []string{filepath.Join(r.dir, journal.RenameName), r.dir} {
		if err := r.flush(p); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
	}
	return nil
}

// triples are a move's paths as the report may hold them, (old, temp, new): the
// frame and its sidecar where they are, and for a frame in a sort folder its home
// paths too (the report keys it by its home).
func (r *run) triples(m journal.RenameMove) [][3]string {
	o, t, n := r.abs(m.Old), r.abs(m.Tmp), r.abs(m.New)
	out := [][3]string{{o, t, n}, {xmp.Path(o), xmp.Path(t), xmp.Path(n)}}
	d := filepath.Dir(o)
	if home := redate.ShootOf(r.dir, d); home != d {
		ho, ht, hn := filepath.Join(home, filepath.Base(o)), filepath.Join(home, filepath.Base(t)), filepath.Join(home, filepath.Base(n))
		out = append(out, [3]string{ho, ht, hn}, [3]string{xmp.Path(ho), xmp.Path(ht), xmp.Path(hn)})
	}
	return out
}

// reportAt is which paths the report holds for j's frames, from the journal and the
// report itself: "" the old ones, "temp", "new". Temp paths are unique to this run, so
// their presence settles which save a crash beat.
func (r *run) reportAt(j *journal.Rename) string {
	if r.rep == nil || j.ReportAt == "new" {
		return "new"
	}
	tmps := map[string]bool{}
	for _, m := range j.Moves {
		for _, t := range r.triples(m) {
			tmps[t[1]] = true
		}
	}
	switch has := r.rep.HasAnyPath(tmps); {
	case has:
		return "temp"
	case j.ReportAt == "temp":
		return "new" // the second save happened
	}
	return ""
}

// followReport moves the report's paths to the new names in two saves (old → temp,
// then temp → new), each flushed and journalled, so frames that swap names never mix.
func (r *run) followReport() error {
	if r.rep == nil {
		return nil
	}
	toTmp, toNew := map[string]string{}, map[string]string{}
	for _, m := range r.j.Moves {
		for _, t := range r.triples(m) {
			toTmp[t[0]], toNew[t[1]] = t[1], t[2]
		}
	}
	at := r.reportAt(r.j)
	filled := r.fillCards()
	if at == "" {
		if r.rep.RenamePaths(toTmp)+filled > 0 {
			if err := r.saveReport(); err != nil {
				return err
			}
		}
		if crash != nil && crash("report1", 0) {
			return errCrash
		}
		r.j.ReportAt = "temp"
		if err := r.saveJournal(); err != nil {
			return err
		}
		if crash != nil && crash("report", 0) {
			return errCrash
		}
		at = "temp"
	}
	if at == "temp" {
		if r.rep.RenamePaths(toNew)+filled > 0 {
			if err := r.saveReport(); err != nil {
				return err
			}
		}
		if crash != nil && crash("report2", 0) {
			return errCrash
		}
	}
	if at == "new" && filled > 0 {
		if err := r.saveReport(); err != nil {
			return err
		}
	}
	r.j.ReportAt = "new"
	return nil
}

// fillCards writes the journal's card names into the report's frames that lack one,
// wherever the report has the frame now (its old, temp or new path), and returns how
// many it filled. Card names don't change with a rename, so filling is idempotent.
func (r *run) fillCards() int {
	if len(r.j.Cards) == 0 {
		return 0
	}
	at := map[string]string{}
	for rel, c := range r.j.Cards {
		at[r.abs(rel)] = c
	}
	for _, m := range r.j.Moves {
		for _, t := range r.triples(m) {
			if c, ok := at[t[0]]; ok {
				at[t[1]], at[t[2]] = c, c
			}
		}
	}
	n := 0
	for i := range r.rep.Results {
		x := &r.rep.Results[i]
		if c, ok := at[x.File]; ok && x.CardName == "" {
			x.CardName = c
			n++
		}
	}
	return n
}

func (r *run) saveReport() error {
	if err := r.rep.Save(r.reportPath); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	r.traced("report save")
	for _, p := range []string{r.reportPath, filepath.Dir(r.reportPath)} {
		if err := r.flush(p); err != nil {
			return fmt.Errorf("report: %w", err)
		}
	}
	return nil
}

// followLabels makes the labels log say, under each new name, what it said under the
// old one when the journal was written (nothing, if nothing: a label some earlier
// frame left under that name is cleared), and clears the old names no frame takes.
// Entries are appended (with From, the old name); the log is never rewritten. Only
// what differs is appended, so a re-run appends nothing twice.
func (r *run) followLabels() error {
	want := map[string]*labels.Entry{}
	from := map[string]string{}
	for _, m := range r.j.Moves {
		want[filepath.Base(m.Old)] = nil
	}
	for _, m := range r.j.Moves {
		want[filepath.Base(m.New)] = m.Label
		from[filepath.Base(m.New)] = filepath.Base(m.Old)
	}
	cur, err := labels.Read(r.labPath)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	now, wrote := time.Now(), false
	for _, n := range names {
		w := want[n]
		got, ok := cur[n]
		var e labels.Entry
		switch {
		case w == nil && !ok:
			continue
		case w == nil:
			e = labels.Entry{File: n, At: now}
		case ok && sameLabel(got, *w):
			continue
		default:
			e = *w
			e.File, e.From, e.At = n, from[n], now
		}
		if err := labels.Append(r.labPath, e); err != nil {
			return err
		}
		r.traced("labels append")
		wrote = true
	}
	if wrote {
		return r.flush(r.labPath)
	}
	return nil
}

func sameLabel(a, b labels.Entry) bool {
	if a.Label != b.Label || a.Stars != b.Stars || (a.EV == nil) != (b.EV == nil) {
		return false
	}
	return a.EV == nil || *a.EV == *b.EV
}

// followManifests appends, for each frame its shoot's manifest records, a superseding
// line under the new name (everything else unchanged), unless its current line already
// has it.
func (r *run) followManifests() error {
	byUnit := map[string][]journal.RenameMove{}
	for _, m := range r.j.Moves {
		if m.Orig != "" {
			u := redate.ShootOf(r.dir, filepath.Dir(r.abs(m.Old)))
			byUnit[u] = append(byUnit[u], m)
		}
	}
	units := make([]string, 0, len(byUnit))
	for u := range byUnit {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, u := range units {
		es, err := offload.CurrentManifest(u)
		if err != nil {
			return err
		}
		grew := false
		for _, m := range byUnit[u] {
			newB := filepath.Base(m.New)
			for _, e := range es {
				// The manifest's own key: the current line per (camera name, size).
				if !strings.EqualFold(e.Orig, m.Orig) || e.Size != m.OrigSize {
					continue
				}
				if e.Name != newB {
					ne := e
					ne.Name = newB
					if err := offload.AppendManifest(u, ne); err != nil {
						return err
					}
					r.traced("manifest append")
					grew = true
				}
				break
			}
		}
		if grew {
			if err := r.flush(filepath.Join(u, offload.ManifestName)); err != nil {
				return err
			}
		}
	}
	return nil
}

// followCache renames the review sheet's cached images (named from the frame's name,
// size, time and focus box) to the names built from the new name, in two passes
// through temp names, so swapped names never collide. A failure is a note: review
// renders what's missing and sweeps what's stale.
func (r *run) followCache() {
	assets := filepath.Join(filepath.Dir(r.reportPath), "cull-review", review.AssetsDir)
	if r.rep == nil {
		return
	}
	if _, err := os.Stat(assets); err != nil {
		return
	}
	// Temps a crash left here, part way through this step: cached images only.
	if ents, err := os.ReadDir(assets); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), offload.RenameTempPrefix) && e.Type().IsRegular() {
				os.Remove(filepath.Join(assets, e.Name()))
			}
		}
	}
	type pair struct{ tmp, to string }
	var pairs []pair
	for _, m := range r.j.Moves {
		t := r.triples(m)
		home := t[0]
		if len(t) > 2 {
			home = t[2]
		}
		var x *report.Result
		for i := range r.rep.Results {
			if r.rep.Results[i].File == home[2] {
				x = &r.rep.Results[i]
			}
		}
		if x == nil {
			continue
		}
		was := *x
		was.File = home[0]
		olds, news := review.CachedImages(r.rep.Dir, was), review.CachedImages(r.rep.Dir, *x)
		for i := range olds {
			if olds[i] == news[i] {
				continue
			}
			tmp := filepath.Join(assets, offload.RenameTempName(olds[i]))
			if err := os.Rename(filepath.Join(assets, olds[i]), tmp); err == nil {
				pairs = append(pairs, pair{tmp, filepath.Join(assets, news[i])})
			}
		}
	}
	failed := 0
	for _, p := range pairs {
		if _, err := os.Lstat(p.to); err == nil || os.Rename(p.tmp, p.to) != nil {
			os.Remove(p.tmp) // a cached image: review renders it again
			failed++
		}
	}
	if failed > 0 {
		r.note("%d cached review image(s) not carried to the new names: cull review renders them again", failed)
	}
}

// flush carries path (a file or folder) to the media: F_FULLFSYNC, fsync on a share.
func (r *run) flush(path string) error {
	r.traced("flush " + path)
	return offload.Flush(path)
}

// move gives a file its next name, never replacing anything.
func (r *run) move(from, to string) error {
	if err := offload.MoveNoReplace(from, to); err != nil {
		return err
	}
	r.traced("move " + r.rel(from) + " -> " + r.rel(to))
	return nil
}

func (r *run) traced(ev string) {
	if trace != nil {
		trace(ev)
	}
}

// otherReports warns about other reports in the folder: their keys don't follow.
func (r *run) otherReports() {
	ents, _ := os.ReadDir(r.dir)
	var others []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "cull-report") && strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, "batch.json") &&
			filepath.Join(r.dir, n) != r.reportPath {
			others = append(others, n)
		}
	}
	if len(others) > 0 {
		r.warn("only %s follows the rename; %s in the folder keep(s) the old names (judge on it would judge the frames again)", filepath.Base(r.reportPath), strings.Join(others, ", "))
	}
}

func (r *run) emit(n ui.Note) {
	if r.o.UI != nil {
		r.o.UI.Emit(ui.Event{Note: &n})
	}
}

// say is a dry run's line, shown even with -q.
func (r *run) say(format string, args ...any) {
	r.emit(ui.Note{Level: ui.Quiet, Text: fmt.Sprintf(format, args...)})
}

func (r *run) note(format string, args ...any) {
	r.emit(ui.Note{Level: ui.Normal, Text: fmt.Sprintf(format, args...)})
}

func (r *run) warn(format string, args ...any) {
	r.emit(ui.Note{Sev: ui.Warn, Text: fmt.Sprintf(format, args...)})
}
