// Package offload copies DNGs off camera cards into a shoot folder, verifying every
// copy against the card from the disk (not the page cache) before it counts. It is
// planned in full before any byte is written, so every refusal comes first.
package offload

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/ui"
)

// Options describe one offload run.
type Options struct {
	Sources  []string // card roots, only ever read
	Dest     string   // root the shoot folder goes under
	Backup   string   // optional second root, same layout
	Name     string   // shoot name: folder "<date> <name>", the {name} token
	Date     string   // YYYY-MM-DD; "" = the earliest capture date in the run
	Rename   string   // pattern with {date} {name} {orig} {n} {n:W}; "" = camera names
	Checksum bool     // skip by SHA-256 rather than size and mtime
	UI       ui.Sink  // progress while planning (reading the cards, --checksum's hashing); nil = none
	// Split makes one shoot folder per event, numbered: a capture-time gap over
	// SplitGap (0 = DefaultSplitGap), or a new day, starts the next. Refused when the
	// capture times can't be trusted (see clockBroken).
	Split    bool
	SplitGap time.Duration
	SplitAt  []string // file names (camera order) that each start a new event; for any clock
	// SetDate fixes every copy's capture dates (EXIF, embedded XMP, file times) to this
	// local time, when SetDateSet; the card is never changed. It dates the shoot folder
	// too: a different Date is refused.
	SetDate    time.Time
	SetDateSet bool

	freeSpace func(path string) (uint64, error) // test hook; nil = statfs
}

// File is one source file and what the run does with it.
type File struct {
	Src     string
	Size    int64
	ModTime time.Time
	Capture time.Time // EXIF capture time, or ModTime without one
	Name    string    // destination base name
	To      []string  // shoot folders it is copied into (those that don't hold it yet)
	Skip    string    // "" = copy; else why not
	// Unverified: a destination already holds it by size and mtime only, with no cull
	// manifest line and no --checksum comparison, so it was never checked against the
	// card and can't count toward "safe to format".
	Unverified bool
	// Undated: skipped as already there, but with --set-date its copies were made
	// without it (the manifest records no dates_set): their dates are the camera's.
	Undated bool
}

// DefaultSplitGap is the capture-time gap that starts a new event with --split.
const DefaultSplitGap = 2 * time.Hour

// Plan is everything an offload will do, decided before it writes anything. A split
// run has one Plan per event.
type Plan struct {
	Event  int      // this plan's event, 1-based, when the run is split into several; else 0
	Folder string   // shoot folder name
	Dests  []string // absolute shoot folders: under Dest and, with Backup, under Backup
	Files  []File
	Bytes  int64  // bytes to copy
	Dated  string // where the folder date came from

	setDate *time.Time // Options.SetDate when set: stage B patches each copy's dates to it
	h       hooks      // test seams for Run
}

// mtimeWindow is the quick check's tolerance: exFAT/FAT timestamps are coarse (FAT
// stores 2 s), as rsync's --modify-window allows for.
const mtimeWindow = 2 * time.Second

// reserve is the free space kept beyond the bytes to copy: 1% plus 512 MB.
func reserve(n int64) uint64 { return uint64(n) + uint64(n)/100 + 512<<20 }

// MakePlan plans an unsplit run (see MakePlans).
func MakePlan(o Options) (*Plan, error) {
	ps, err := MakePlans(o)
	if len(ps) == 0 {
		return nil, err
	}
	return ps[0], err
}

// MakePlans scans the sources, splits them into events (Split, SplitAt; one event
// otherwise) and plans each into its own shoot folder, deciding every name, skip and
// refusal. It writes nothing. When the only problem is free space, checked for every
// event together, the plans come back with the error, so a dry run can still show it.
func MakePlans(o Options) ([]*Plan, error) {
	if len(o.Sources) == 0 || o.Dest == "" {
		return nil, errors.New("offload needs at least one source and a destination")
	}
	if o.Split && len(o.SplitAt) > 0 {
		return nil, errors.New("--split finds the events by capture time and --split-at names them yourself: pick one")
	}
	if o.SetDateSet {
		day := o.SetDate.Format(time.DateOnly)
		if o.Date != "" && o.Date != day {
			return nil, fmt.Errorf("--set-date %s dates the shoot folder: --date %s disagrees (drop --date)", day, o.Date)
		}
		if y := o.SetDate.Year(); y < 1 || y > 9999 {
			return nil, fmt.Errorf("--set-date %s: the year must have 4 digits", day)
		}
	}
	files, err := scan(o.Sources, o.UI)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no DNGs found under %s", strings.Join(o.Sources, ", "))
	}
	events, err := partition(files, o)
	if err != nil {
		return nil, err
	}
	var plans []*Plan
	for i, ev := range events {
		n := 0
		if len(events) > 1 {
			n = i + 1
		}
		p, err := planEvent(o, ev, n)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, checkSpace(plans, o)
}

// planEvent plans one event's files into its shoot folder; n > 0 numbers the folder.
func planEvent(o Options, files []File, n int) (*Plan, error) {
	p := &Plan{Files: files, Event: n}
	if o.SetDateSet {
		t := o.SetDate
		p.setDate = &t
	}
	if err := p.date(o); err != nil {
		return nil, err
	}
	p.Dests = []string{filepath.Join(o.Dest, p.Folder)}
	if o.Backup != "" {
		p.Dests = append(p.Dests, filepath.Join(o.Backup, p.Folder))
	}
	var err error
	if o.Rename != "" {
		err = p.renamed(o)
	} else {
		err = p.cameraNames(o)
	}
	if err != nil {
		return nil, err
	}
	for _, f := range p.Files {
		if f.Skip == "" {
			p.Bytes += f.Size
		}
	}
	return p, nil
}

// partition splits the scanned files into events, each in camera-name order.
func partition(files []File, o Options) ([][]File, error) {
	switch {
	case len(o.SplitAt) > 0:
		starts := map[string]bool{}
		for _, n := range o.SplitAt {
			starts[stem(n)] = true
		}
		var events [][]File
		for i, f := range files { // files are in camera-name order
			if i == 0 || starts[stem(f.Src)] {
				events = append(events, nil)
			}
			delete(starts, stem(f.Src))
			events[len(events)-1] = append(events[len(events)-1], f)
		}
		if len(starts) > 0 {
			var missing []string
			for _, n := range o.SplitAt {
				if starts[stem(n)] {
					missing = append(missing, n)
				}
			}
			return nil, fmt.Errorf("--split-at %s: no such file on the cards", strings.Join(missing, ", "))
		}
		return events, nil
	case o.Split:
		if same, pairs, broken := clockBroken(files); broken {
			return nil, fmt.Errorf("capture times can't place the split: %d of %d consecutive frames share a timestamp "+
				"(a camera clock that wasn't running?); name the first file of each later event instead, e.g. --split-at %s",
				same, pairs, filepath.Base(files[len(files)/2].Src))
		}
		gap := o.SplitGap
		if gap <= 0 {
			gap = DefaultSplitGap
		}
		byTime := slices.Clone(files)
		sort.SliceStable(byTime, func(i, j int) bool { return byTime[i].Capture.Before(byTime[j].Capture) })
		var events [][]File
		for i, f := range byTime {
			if i == 0 || f.Capture.Sub(byTime[i-1].Capture) > gap ||
				f.Capture.Format("2006-01-02") != byTime[i-1].Capture.Format("2006-01-02") {
				events = append(events, nil)
			}
			events[len(events)-1] = append(events[len(events)-1], f)
		}
		for _, ev := range events {
			sort.Slice(ev, func(i, j int) bool { return lessByName(ev[i], ev[j]) })
		}
		return events, nil
	}
	return [][]File{files}, nil
}

// stem is a file name without its folder or extension, lower-cased: how --split-at
// names match ("M1103402", "m1103402.dng").
func stem(p string) string {
	b := filepath.Base(p)
	return strings.ToLower(strings.TrimSuffix(b, filepath.Ext(b)))
}

// clockBroken reports capture times too uniform to split on: at least 90% of
// consecutive frames (in camera order, at least ten pairs) share a timestamp. A
// burst puts frames in one second, but not nearly all of a card; a clock that
// wasn't running does (seen: 969 of 991 on an M11-P card).
func clockBroken(files []File) (same, pairs int, broken bool) {
	for i := 1; i < len(files); i++ {
		pairs++
		if files[i].Capture.Truncate(time.Second).Equal(files[i-1].Capture.Truncate(time.Second)) {
			same++
		}
	}
	return same, pairs, pairs >= 10 && same*10 >= pairs*9
}

// checkSpace makes sure each volume has room for every copy it will receive: the
// destination and the backup on one drive need room for both, and so do all the
// events of a split run.
func checkSpace(plans []*Plan, o Options) error {
	free := o.freeSpace
	if free == nil {
		free = statfsFree
	}
	roots := []string{o.Dest}
	if o.Backup != "" {
		roots = append(roots, o.Backup)
	}
	type volume struct {
		roots []string
		need  int64
	}
	var vols []*volume
	byID := map[uint64]*volume{}
	for i, r := range roots {
		var need int64
		for _, p := range plans {
			for _, f := range p.Files {
				if slices.Contains(f.To, p.Dests[i]) {
					need += f.Size
				}
			}
		}
		id, err := volumeID(r)
		if err != nil {
			return fmt.Errorf("volume of %s: %w", r, err)
		}
		v := byID[id]
		if v == nil {
			v = &volume{}
			byID[id] = v
			vols = append(vols, v)
		}
		v.roots = append(v.roots, r)
		v.need += need
	}
	for _, v := range vols {
		n, err := free(v.roots[0])
		if err != nil {
			return fmt.Errorf("free space on %s: %w", v.roots[0], err)
		}
		if need := reserve(v.need); n < need {
			return fmt.Errorf("not enough free space for %s: %s free, %s needed (%s to copy there plus a margin)",
				strings.Join(v.roots, " and "), gb(int64(n)), gb(int64(need)), gb(v.need))
		}
	}
	return nil
}

// scan lists every regular .dng file under the sources, never following symlinks and
// skipping dot-files and dot-directories (.Trashes, .Spotlight-V100, .fseventsd,
// AppleDouble ._ files), in camera-name order: file numbers are the camera's own
// order, and unlike capture times they survive a clock set wrong.
func scan(sources []string, s ui.Sink) ([]File, error) {
	t := ui.Track(s, "plan", "reading the cards", "files", 0)
	defer t.Done()
	var out []File
	for _, root := range sources {
		root, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("%s is not a readable folder", root)
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("read %s: %w", p, err)
			}
			if p != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !strings.EqualFold(filepath.Ext(p), ".dng") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("read %s: %w", p, err)
			}
			f := File{Src: p, Size: info.Size(), ModTime: info.ModTime(), Capture: info.ModTime()}
			if e, err := dng.ReadExif(p); err == nil {
				if t, ok := e.CaptureTime(); ok {
					f.Capture = t
				}
			}
			out = append(out, f)
			t.Add(1)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessByName(out[i], out[j]) })
	return out, nil
}

// lessByName is camera order: by file name, then full path.
func lessByName(a, b File) bool {
	ba, bb := filepath.Base(a.Src), filepath.Base(b.Src)
	if ba != bb {
		return ba < bb
	}
	return a.Src < b.Src
}

func (p *Plan) date(o Options) error {
	day := o.Date
	if o.SetDateSet {
		day = o.SetDate.Format(time.DateOnly)
		p.Dated = "--set-date"
	} else if day != "" {
		if _, err := time.Parse("2006-01-02", day); err != nil {
			return fmt.Errorf("--date %q: want YYYY-MM-DD", day)
		}
		p.Dated = "--date"
	} else {
		first := p.Files[0]
		for _, f := range p.Files[1:] {
			if f.Capture.Before(first.Capture) {
				first = f
			}
		}
		day = first.Capture.Format("2006-01-02")
		p.Dated = "earliest capture date, " + filepath.Base(first.Src)
	}
	p.Folder = day
	if o.Name != "" {
		if strings.ContainsAny(o.Name, `/\:`) {
			return fmt.Errorf("--name %q: no path separators or colons", o.Name)
		}
		p.Folder += " " + o.Name
	}
	if p.Event > 0 {
		p.Folder += " " + strconv.Itoa(p.Event)
	}
	return nil
}

// existing is what a destination folder already holds under one name: in the shoot
// folder itself, or moved by --sort / --move-culled into one of MovedDirs.
func existing(dir, name string) (string, os.FileInfo, bool) {
	for _, sub := range append([]string{""}, MovedDirs...) {
		p := filepath.Join(dir, sub, name)
		if st, err := os.Lstat(p); err == nil {
			return p, st, true
		}
	}
	return "", nil, false
}

// same reports whether the file already at path is f: by SHA-256 with checksum, else
// by size and mtime within the window.
func same(f File, path string, st os.FileInfo, checksum bool) (bool, error) {
	if !st.Mode().IsRegular() || st.Size() != f.Size {
		return false, nil
	}
	if !checksum {
		d := st.ModTime().Sub(f.ModTime)
		return d <= mtimeWindow && d >= -mtimeWindow, nil
	}
	a, err := fileSum(f.Src)
	if err != nil {
		return false, err
	}
	// The copy is checked from the disk, as the engine checks its own: a file copied
	// minutes ago would otherwise be compared against the page cache.
	b, err := hashFromDisk(context.Background(), path)
	return a == b, err
}

// recordedSame is same for a copy whose capture dates were set (offload --set-date,
// redate): its bytes and mtime differ from the card's on purpose, so the card file is
// matched against the manifest entry e instead. e applies when it records dates_set
// for this card file (orig, size and mtime, within the window); then the copy is f
// when its size is the card's and, as the quick check, its mtime is the date set
// (within the window); with checksum, when the card hashes to e's sha256 and the
// copy, read from the disk, to e's file_sha256 (sha256 when only its file times were
// set). applies false: e says nothing about f, and same decides.
func recordedSame(f File, e Entry, path string, st os.FileInfo, checksum bool) (applies, eq bool, err error) {
	dt := e.ModTime.Sub(f.ModTime)
	if e.DatesSet == "" || !strings.EqualFold(e.Orig, filepath.Base(f.Src)) || e.Size != f.Size ||
		dt > mtimeWindow || dt < -mtimeWindow {
		return false, false, nil
	}
	if !st.Mode().IsRegular() || st.Size() != f.Size {
		return true, false, nil
	}
	if !checksum {
		set, err := time.ParseInLocation("2006-01-02T15:04:05", e.DatesSet, time.Local)
		if err != nil {
			return true, false, nil
		}
		d := st.ModTime().Sub(set)
		return true, d <= mtimeWindow && d >= -mtimeWindow, nil
	}
	eq, err = matchesEntry(f, e, path)
	return true, eq, err
}

// matchesEntry is the --checksum comparison against a manifest entry: the card file
// hashes to e's sha256 and the copy at path, read from the disk, to the checksum the
// file must have now (file_sha256 when its dates were patched, else sha256).
func matchesEntry(f File, e Entry, path string) (bool, error) {
	a, err := fileSum(f.Src)
	if err != nil {
		return false, err
	}
	if hexOf(a[:]) != e.SHA256 {
		return false, nil
	}
	b, err := hashFromDisk(context.Background(), path)
	return err == nil && hexOf(b[:]) == e.CurrentSHA256(), err
}

// sameDated is same for a copy --set-date made that no manifest records: a crash
// between naming it and appending its manifest line. Its size is the card's and its
// mtime the date being set; with checksum, it must hash, read from the disk, to the
// card's bytes with that date's patches applied. Without checksum it is only alike,
// and cameraNames marks it unverified, as any copy the manifest doesn't vouch for.
func sameDated(f File, path string, st os.FileInfo, checksum bool, t time.Time) (bool, error) {
	if !st.Mode().IsRegular() || st.Size() != f.Size {
		return false, nil
	}
	if !checksum {
		d := st.ModTime().Sub(t)
		return d <= mtimeWindow && d >= -mtimeWindow, nil
	}
	in, err := os.Open(f.Src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	ps, _, err := dng.PatchDates(in, f.Size, t)
	if err != nil {
		return false, nil // no dates to set: same already compared it with the card
	}
	_, want, err := streamPatched(context.Background(), in, ps, nil)
	if err != nil {
		return false, err
	}
	got, err := hashFromDisk(context.Background(), path)
	return err == nil && got == want, err
}

func fileSum(p string) ([32]byte, error) {
	var s [32]byte
	f, err := os.Open(p)
	if err != nil {
		return s, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return s, err
	}
	copy(s[:], h.Sum(nil))
	return s, nil
}

// cameraNames keeps the camera's file names: a name already present is skipped when it
// is the same file on every destination, and refused when it differs on any.
func (p *Plan) cameraNames(o Options) error {
	verified := make([]map[string]int64, len(p.Dests)) // per destination: manifest name → size
	dated := make([]map[string]Entry, len(p.Dests))    // per destination: lower-case name → current entry with dates_set
	for i, d := range p.Dests {
		man, err := readManifest(d)
		if err != nil {
			return err
		}
		verified[i] = map[string]int64{}
		for _, e := range man {
			verified[i][e.Name] = e.Size
		}
		dated[i] = map[string]Entry{}
		for _, e := range current(man) {
			if e.DatesSet != "" {
				dated[i][strings.ToLower(e.Name)] = e
			}
		}
	}
	byName := map[string]string{}
	var t *ui.Tracker
	if o.Checksum { // every file already there is hashed on the card and on the disk
		t = ui.Track(o.UI, "checksum", "comparing what's already copied by SHA-256", "files", len(p.Files))
		defer t.Done()
	}
	for i := range p.Files {
		f := &p.Files[i]
		t.Add(1)
		f.Name = filepath.Base(f.Src)
		key := strings.ToLower(f.Name)
		if other, ok := byName[key]; ok {
			return fmt.Errorf("%s is on two sources (%s and %s): use --rename to keep both", f.Name, other, f.Src)
		}
		byName[key] = f.Src
		datedThere := false // a copy already there is a dated one
		for i, d := range p.Dests {
			at, st, ok := existing(d, f.Name)
			if !ok {
				f.To = append(f.To, d)
				continue
			}
			// A copy whose dates were set no longer matches the card by mtime or hash:
			// its manifest entry vouches for it instead.
			applies, eq, err := false, false, error(nil)
			if e, ok := dated[i][key]; ok {
				applies, eq, err = recordedSame(*f, e, at, st, o.Checksum)
				datedThere = datedThere || (applies && eq)
			}
			if !applies && err == nil {
				eq, err = same(*f, at, st, o.Checksum)
				// Only a name no manifest line records: one that does, and doesn't match
				// above, is another file.
				if _, recorded := verified[i][f.Name]; !eq && err == nil && p.setDate != nil && !recorded {
					eq, err = sameDated(*f, at, st, o.Checksum, *p.setDate)
					datedThere = datedThere || eq
				}
			}
			if err != nil {
				return err
			}
			if !eq {
				return fmt.Errorf("%s exists with different content; use --rename to keep both", at)
			}
			if size, ok := verified[i][f.Name]; !o.Checksum && (!ok || size != f.Size) {
				f.Unverified = true
			}
		}
		if len(f.To) == 0 {
			f.Skip = "already copied"
			f.Undated = p.setDate != nil && !datedThere
		}
	}
	return nil
}

var tokenRE = regexp.MustCompile(`\{(date|name|orig|n)(?::(\d+))?\}`)

// ValidatePattern checks a file-name pattern (offload --rename, cull rename): the
// tokens {date} {name} {orig} {n} {n:W}, with {n} or {orig} so that every file gets
// its own name, no path separators or colons, and no leading "." (a hidden file is
// never a frame). Errors start with the pattern, quoted.
func ValidatePattern(pat string) error {
	if !patternNumbered(pat) && !strings.Contains(pat, "{orig}") {
		return fmt.Errorf("%q needs {n} or {orig}, or every file gets the same name", pat)
	}
	if strings.ContainsAny(tokenRE.ReplaceAllString(pat, ""), `/\:`) {
		return fmt.Errorf("%q: no path separators or colons", pat)
	}
	for _, m := range tokenRE.FindAllStringSubmatch(pat, -1) {
		if m[2] != "" && m[1] != "n" {
			return fmt.Errorf("%q: only {n} takes a width", pat)
		}
	}
	if rest := tokenRE.ReplaceAllString(pat, ""); strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("%q: unknown token (use {date} {name} {orig} {n} {n:W})", pat)
	}
	if strings.HasPrefix(pat, ".") {
		return fmt.Errorf("%q: a name starting with \".\" is hidden, and never taken for a frame", pat)
	}
	return nil
}

func patternNumbered(pat string) bool {
	for _, m := range tokenRE.FindAllStringSubmatch(pat, -1) {
		if m[1] == "n" {
			return true
		}
	}
	return false
}

// ExpandName fills pattern's tokens: {date} (YYYYMMDD), {name} (spaces become "_"),
// {orig} (the camera name's stem) and {n} ({n:W}: zero-padded to W digits). The result
// has no extension. The pattern is checked with ValidatePattern first.
func ExpandName(pattern string, date, name, orig string, n int) (string, error) {
	if err := ValidatePattern(pattern); err != nil {
		return "", err
	}
	name = strings.ReplaceAll(name, " ", "_")
	return tokenRE.ReplaceAllStringFunc(pattern, func(tok string) string {
		m := tokenRE.FindStringSubmatch(tok)
		switch m[1] {
		case "date":
			return date
		case "name":
			return name
		case "orig":
			return orig
		}
		w, _ := strconv.Atoi(m[2])
		return fmt.Sprintf("%0*d", w, n)
	}), nil
}

// renamed names files by the pattern, numbering in camera-name order from one past the
// largest number already in the shoot folder. Files the manifest already records
// (same source name, size and mtime) are skipped under their recorded name.
func (p *Plan) renamed(o Options) error {
	pat := o.Rename
	if err := ValidatePattern(pat); err != nil {
		return fmt.Errorf("--rename %w", err)
	}
	numbered := patternNumbered(pat)
	date := strings.ReplaceAll(p.Folder[:10], "-", "")
	name := strings.ReplaceAll(o.Name, " ", "_")
	expand := func(orig string, n int) string {
		s, _ := ExpandName(pat, date, name, orig, n) // validated above
		return s
	}
	// The counter continues from the largest n among names the pattern produced, in the
	// shoot folder and in the folders --sort and --move-culled move frames into.
	re := patternRE(pat, date, name)
	next := 1
	recorded := make([]map[string]Entry, len(p.Dests)) // per destination: orig+size → manifest line
	seenNames := map[string]bool{}
	for i, d := range p.Dests {
		for _, sub := range append([]string{""}, MovedDirs...) {
			ents, err := os.ReadDir(filepath.Join(d, sub))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			for _, e := range ents {
				seenNames[strings.ToLower(e.Name())] = true
				if m := re.FindStringSubmatch(e.Name()); numbered && len(m) > 1 && m[1] != "" {
					if n, err := strconv.Atoi(m[1]); err == nil && n >= next {
						next = n + 1
					}
				}
			}
		}
		man, err := readManifest(d)
		if err != nil {
			return err
		}
		recorded[i] = map[string]Entry{}
		for _, e := range man {
			recorded[i][strings.ToLower(e.Orig)+"\x00"+strconv.FormatInt(e.Size, 10)] = e
		}
	}
	var t *ui.Tracker
	if o.Checksum {
		t = ui.Track(o.UI, "checksum", "comparing what's already copied by SHA-256", "files", len(p.Files))
		defer t.Done()
	}
	for i := range p.Files {
		f := &p.Files[i]
		t.Add(1)
		orig := filepath.Base(f.Src)
		key := strings.ToLower(orig) + "\x00" + strconv.FormatInt(f.Size, 10)
		matches := func(e Entry) bool {
			dt := e.ModTime.Sub(f.ModTime)
			return dt <= mtimeWindow && dt >= -mtimeWindow
		}
		var rec *Entry
		for _, m := range recorded {
			if e, ok := m[key]; ok && matches(e) {
				rec = &e
				break
			}
		}
		if rec != nil {
			// Copied before under rec.Name: it is skipped only where it still is, and
			// counts as verified only where that folder's manifest records it.
			f.Name = rec.Name
			for j, d := range p.Dests {
				at, st, ok := existing(d, rec.Name)
				if !ok {
					f.To = append(f.To, d)
					continue
				}
				if !st.Mode().IsRegular() || st.Size() != f.Size {
					return fmt.Errorf("%s isn't the copy the manifest records (size %d, the card's is %d); move it aside and rerun",
						at, st.Size(), f.Size)
				}
				if e, ok := recorded[j][key]; ok && e.Name == rec.Name && matches(e) {
					// Recorded here: verified when it was made. --checksum checks it
					// still is, against the checksums that line records.
					if o.Checksum {
						eq, err := matchesEntry(*f, e, at)
						if err != nil {
							return err
						}
						if !eq {
							return fmt.Errorf("%s exists with different content than the manifest records; move it aside and rerun", at)
						}
					}
					continue
				}
				eq := false
				if o.Checksum {
					applies, ok, err := recordedSame(*f, *rec, at, st, true)
					if !applies && err == nil {
						ok, err = same(*f, at, st, true)
					}
					if err != nil {
						return err
					}
					eq = ok
				}
				if !eq {
					f.Unverified = true
				}
			}
			if len(f.To) == 0 {
				f.Skip = "in manifest"
				f.Undated = p.setDate != nil && rec.DatesSet == ""
			}
			continue
		}
		stem := strings.TrimSuffix(orig, filepath.Ext(orig))
		f.Name = expand(stem, next) + strings.ToUpper(filepath.Ext(orig))
		if numbered {
			next++
		}
		if seenNames[strings.ToLower(f.Name)] {
			return fmt.Errorf("%s already exists in the shoot folder; pick a pattern with {n}", f.Name)
		}
		seenNames[strings.ToLower(f.Name)] = true
		f.To = p.Dests
	}
	return nil
}

// patternRE matches names the pattern produced for this folder, capturing {n}.
func patternRE(pat, date, name string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range tokenRE.FindAllStringSubmatchIndex(pat, -1) {
		b.WriteString(regexp.QuoteMeta(pat[last:loc[0]]))
		switch pat[loc[2]:loc[3]] {
		case "date":
			b.WriteString(regexp.QuoteMeta(date))
		case "name":
			b.WriteString(regexp.QuoteMeta(name))
		case "orig":
			b.WriteString(`.+?`)
		case "n":
			b.WriteString(`(\d+)`)
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(pat[last:]))
	b.WriteString(`\.[A-Za-z0-9]+$`)
	return regexp.MustCompile(b.String())
}

// volumeID identifies the volume path is on (or will be created on: the nearest
// existing ancestor's).
func volumeID(path string) (uint64, error) {
	p := filepath.Clean(path)
	for {
		st, err := os.Stat(p)
		if err == nil {
			return uint64(st.Sys().(*syscall.Stat_t).Dev), nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return 0, err
		}
		p = parent
	}
}

// statfsFree is the space free to unprivileged users on path's volume; a path that
// doesn't exist yet (the shoot folder's parents) is measured at its nearest existing
// ancestor, the volume it will be created on.
func statfsFree(path string) (uint64, error) {
	p := filepath.Clean(path)
	for {
		var s syscall.Statfs_t
		err := syscall.Statfs(p, &s)
		if err == nil {
			return s.Bavail * uint64(s.Bsize), nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return 0, err
		}
		p = parent
	}
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// Summary is the plan as text: what --dry-run prints.
func (p *Plan) Summary() string {
	var b strings.Builder
	copyN := 0
	skips := map[string]int{}
	var first, last string
	for _, f := range p.Files {
		if f.Skip != "" {
			skips[f.Skip]++
			continue
		}
		copyN++
		if first == "" {
			first = f.Name
		}
		last = f.Name
	}
	fmt.Fprintf(&b, "shoot folder: %s (date from %s)\n", p.Folder, p.Dated)
	for _, d := range p.Dests {
		fmt.Fprintf(&b, "  into %s\n", d)
	}
	fmt.Fprintf(&b, "%d of %d DNGs to copy, %s", copyN, len(p.Files), gb(p.Bytes))
	if copyN > 0 {
		fmt.Fprintf(&b, " (%s … %s)", first, last)
	}
	b.WriteString("\n")
	for _, k := range []string{"already copied", "in manifest"} {
		if n := skips[k]; n > 0 {
			fmt.Fprintf(&b, "skipping %d %s\n", n, k)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
