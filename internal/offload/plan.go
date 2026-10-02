// Package offload copies DNGs off camera cards into a shoot folder, verifying every
// copy against the card from the disk (not the page cache) before it counts. It is
// planned in full before any byte is written, so every refusal comes first.
package offload

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
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
}

// Plan is everything an offload will do, decided before it writes anything.
type Plan struct {
	Folder string   // shoot folder name
	Dests  []string // absolute shoot folders: under Dest and, with Backup, under Backup
	Files  []File
	Bytes  int64  // bytes to copy
	Dated  string // where the folder date came from

	h hooks // test seams for Run
}

// mtimeWindow is the quick check's tolerance: exFAT/FAT timestamps are coarse (FAT
// stores 2 s), as rsync's --modify-window allows for.
const mtimeWindow = 2 * time.Second

// reserve is the free space kept beyond the bytes to copy: 1% plus 512 MB.
func reserve(n int64) uint64 { return uint64(n) + uint64(n)/100 + 512<<20 }

// MakePlan scans the sources and decides every name, skip and refusal. It writes nothing.
func MakePlan(o Options) (*Plan, error) {
	if len(o.Sources) == 0 || o.Dest == "" {
		return nil, errors.New("offload needs at least one source and a destination")
	}
	files, err := scan(o.Sources)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no DNGs found under %s", strings.Join(o.Sources, ", "))
	}
	p := &Plan{Files: files}
	if err := p.date(o); err != nil {
		return nil, err
	}
	p.Dests = []string{filepath.Join(o.Dest, p.Folder)}
	if o.Backup != "" {
		p.Dests = append(p.Dests, filepath.Join(o.Backup, p.Folder))
	}
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
	free := o.freeSpace
	if free == nil {
		free = statfsFree
	}
	roots := []string{o.Dest}
	if o.Backup != "" {
		roots = append(roots, o.Backup)
	}
	for _, r := range roots {
		n, err := free(r)
		if err != nil {
			return nil, fmt.Errorf("free space on %s: %w", r, err)
		}
		if need := reserve(p.Bytes); n < need {
			return nil, fmt.Errorf("not enough free space on %s: %s free, %s needed (%s to copy plus a margin)",
				r, gb(int64(n)), gb(int64(need)), gb(p.Bytes))
		}
	}
	return p, nil
}

// scan lists every regular .dng file under the sources, never following symlinks and
// skipping dot-files and dot-directories (.Trashes, .Spotlight-V100, .fseventsd,
// AppleDouble ._ files), in camera-name order: file numbers are the camera's own
// order, and unlike capture times they survive a clock set wrong.
func scan(sources []string) ([]File, error) {
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
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		bi, bj := filepath.Base(out[i].Src), filepath.Base(out[j].Src)
		if bi != bj {
			return bi < bj
		}
		return out[i].Src < out[j].Src
	})
	return out, nil
}

func (p *Plan) date(o Options) error {
	day := o.Date
	if day != "" {
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
	return nil
}

// existing is what a destination folder already holds under one name.
func existing(dir, name string) (os.FileInfo, bool) {
	st, err := os.Lstat(filepath.Join(dir, name))
	return st, err == nil
}

// same reports whether the file already at dir/name is f: by SHA-256 with checksum,
// else by size and mtime within the window.
func same(f File, dir, name string, st os.FileInfo, checksum bool) (bool, error) {
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
	b, err := fileSum(filepath.Join(dir, name))
	return a == b, err
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
	for i, d := range p.Dests {
		man, err := readManifest(d)
		if err != nil {
			return err
		}
		verified[i] = map[string]int64{}
		for _, e := range man {
			verified[i][e.Name] = e.Size
		}
	}
	byName := map[string]string{}
	for i := range p.Files {
		f := &p.Files[i]
		f.Name = filepath.Base(f.Src)
		key := strings.ToLower(f.Name)
		if other, ok := byName[key]; ok {
			return fmt.Errorf("%s is on two sources (%s and %s): use --rename to keep both", f.Name, other, f.Src)
		}
		byName[key] = f.Src
		for i, d := range p.Dests {
			st, ok := existing(d, f.Name)
			if !ok {
				f.To = append(f.To, d)
				continue
			}
			eq, err := same(*f, d, f.Name, st, o.Checksum)
			if err != nil {
				return err
			}
			if !eq {
				return fmt.Errorf("%s exists with different content; use --rename to keep both", filepath.Join(d, f.Name))
			}
			if size, ok := verified[i][f.Name]; !o.Checksum && (!ok || size != f.Size) {
				f.Unverified = true
			}
		}
		if len(f.To) == 0 {
			f.Skip = "already copied"
		}
	}
	return nil
}

var tokenRE = regexp.MustCompile(`\{(date|name|orig|n)(?::(\d+))?\}`)

// renamed names files by the pattern, numbering in camera-name order from one past the
// largest number already in the shoot folder. Files the manifest already records
// (same source name, size and mtime) are skipped under their recorded name.
func (p *Plan) renamed(o Options) error {
	pat := o.Rename
	numbered := false
	for _, m := range tokenRE.FindAllStringSubmatch(pat, -1) {
		numbered = numbered || m[1] == "n"
	}
	if !numbered && !strings.Contains(pat, "{orig}") {
		return fmt.Errorf("--rename %q needs {n} or {orig}, or every file gets the same name", pat)
	}
	if strings.ContainsAny(tokenRE.ReplaceAllString(pat, ""), `/\:`) {
		return fmt.Errorf("--rename %q: no path separators or colons", pat)
	}
	for _, m := range tokenRE.FindAllStringSubmatch(pat, -1) {
		if m[2] != "" && m[1] != "n" {
			return fmt.Errorf("--rename %q: only {n} takes a width", pat)
		}
	}
	if rest := tokenRE.ReplaceAllString(pat, ""); strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("--rename %q: unknown token (use {date} {name} {orig} {n} {n:W})", pat)
	}
	date := strings.ReplaceAll(p.Folder[:10], "-", "")
	name := strings.ReplaceAll(o.Name, " ", "_")
	expand := func(orig string, n int) string {
		return tokenRE.ReplaceAllStringFunc(pat, func(tok string) string {
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
		})
	}
	// The counter continues from the largest n among names the pattern produced.
	re := patternRE(pat, date, name)
	next := 1
	recorded := map[string]Entry{}
	seenNames := map[string]bool{}
	for _, d := range p.Dests {
		ents, err := os.ReadDir(d)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, e := range ents {
			seenNames[strings.ToLower(e.Name())] = true
			if m := re.FindStringSubmatch(e.Name()); m != nil && m[1] != "" {
				if n, err := strconv.Atoi(m[1]); err == nil && n >= next {
					next = n + 1
				}
			}
		}
		man, err := readManifest(d)
		if err != nil {
			return err
		}
		if d == p.Dests[0] {
			for _, e := range man {
				recorded[strings.ToLower(e.Orig)+"\x00"+strconv.FormatInt(e.Size, 10)] = e
			}
		}
	}
	for i := range p.Files {
		f := &p.Files[i]
		orig := filepath.Base(f.Src)
		if e, ok := recorded[strings.ToLower(orig)+"\x00"+strconv.FormatInt(f.Size, 10)]; ok {
			if dt := e.ModTime.Sub(f.ModTime); dt <= mtimeWindow && dt >= -mtimeWindow {
				f.Name, f.Skip = e.Name, "in manifest"
				continue
			}
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

func statfsFree(path string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil
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
