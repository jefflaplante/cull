package dng

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"
)

const (
	tagDateTime          = 0x0132
	tagXMLPacket         = 0x02BC
	tagDateTimeDigitized = 0x9004
	tagSubSecTime        = 0x9290
	tagSubSecDigitized   = 0x9292
	tagC2PA              = 0xCD41 // C2PA Content Credentials manifest (JUMBF), UNDEFINED

	typeUndefined = 7

	maxXMPPacket = 1 << 20
	maxSubSec    = 64
)

// Patch is one same-length rewrite of a file's bytes.
type Patch struct {
	Off      int64  // file offset
	Old, New []byte // len(Old) == len(New)
	What     string // e.g. "EXIF DateTimeOriginal", "XMP xmp:CreateDate"
}

// xmpDateProps are the embedded-XMP properties PatchDates rewrites, by their
// conventional prefixes.
var xmpDateProps = []string{
	"xmp:CreateDate", "xmp:ModifyDate", "xmp:MetadataDate",
	"exif:DateTimeOriginal", "exif:DateTimeDigitized", "photoshop:DateCreated",
}

// xmpPropRes holds, per property, the attribute form (prop="v" or prop='v', after
// whitespace) and the element form (<prop ...>v</prop>). Each has one non-empty
// value group.
var xmpPropRes = func() map[string][2]*regexp.Regexp {
	m := map[string][2]*regexp.Regexp{}
	for _, p := range xmpDateProps {
		q := regexp.QuoteMeta(p)
		m[p] = [2]*regexp.Regexp{
			regexp.MustCompile(`(?:^|\s)` + q + `\s*=\s*(?:"([^"]*)"|'([^']*)')`),
			regexp.MustCompile(`<` + q + `(?:\s[^>]*)?>([^<]*)</` + q + `\s*>`),
		}
	}
	return m
}()

// xmpDateName finds any prefixed occurrence of the six properties' local names,
// so a form PatchDates doesn't rewrite (another prefix, an rdf:Seq-wrapped value)
// is reported rather than silently missed.
var xmpDateName = regexp.MustCompile(`([A-Za-z_][\w.-]*):(?:CreateDate|ModifyDate|MetadataDate|DateTimeOriginal|DateTimeDigitized|DateCreated)\b`)

// ContentCredentialsSkip is PatchDates' one skipped entry for a frame that carries
// C2PA Content Credentials.
const ContentCredentialsSkip = "C2PA Content Credentials: left unchanged so its signature stays valid (it carries its own signed capture dates)"

var (
	// isoDate: YYYY-MM-DD, then optionally THH:MM, :SS, .f+, and Z or ±HH:MM (only
	// after a time). Groups: 1 date, 2 HH:MM, 3 SS, 4 fraction digits.
	isoDate  = regexp.MustCompile(`^(\d{4}-\d\d-\d\d)(?:T(\d\d:\d\d)(?::(\d\d)(?:\.(\d+))?)?(?:Z|[+-]\d\d:\d\d)?)?$`)
	exifDate = regexp.MustCompile(`^\d{4}:\d\d:\d\d \d\d:\d\d:\d\d$`)
)

// PatchDates returns the same-length patches that set every capture date in the DNG
// read from r (size bytes) to t (sub-seconds zeroed), sorted by Off and
// non-overlapping. Fields already equal to the target are omitted, so a second call
// on a patched file returns none. skipped names embedded-XMP values it could not
// rewrite at the same length (left alone). It writes nothing.
//
// The fields (spec §2): IFD0 DateTime (0x0132); DateTimeOriginal and
// DateTimeDigitized (0x9003, 0x9004) and SubSecTime* (0x9290–0x9292) in the Exif
// IFD, and in IFD0 too where a writer put them there (ReadExif reads IFD0's
// DateTimeOriginal); and the date values of the XMP packet in IFD0's 0x02BC. The
// date-times take t's wall clock in t's location; sub-second digits become '0'. An
// ASCII date that is neither a date nor blank (spaces, NULs, EXIF's "    :  :     :  :  "),
// and an XMP value that isn't ISO 8601, are reported in skipped as
// "<What>: unparseable" and left alone. skipped is ordered by file offset.
// Time-zone fields (OffsetTime*) and every other byte are never touched.
//
// A frame with C2PA Content Credentials (IFD0 tag 0xCD41) is never patched: its
// signed manifest repeats the capture dates and its hash binding covers the date
// fields, so any patch would break the signature. PatchDates returns no patches
// and the single skipped entry ContentCredentialsSkip for it.
func PatchDates(r io.ReaderAt, size int64, t time.Time) (patches []Patch, skipped []string, err error) {
	if y := t.Year(); y < 1 || y > 9999 {
		return nil, nil, fmt.Errorf("date %s: the year must have 4 digits to keep every field's length", t.Format(time.DateOnly))
	}
	tr, ifd0Off, err := newTIFFReader(r, size)
	if err != nil {
		return nil, nil, err
	}
	ifd0, _, err := tr.readIFD(ifd0Off)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := ifd0[tagC2PA]; ok {
		return nil, []string{ContentCredentialsSkip}, nil
	}
	p := &patcher{t: tr, size: size, exifDate: []byte(t.Format("2006:01:02 15:04:05")), when: t}
	p.ascii(ifd0, tagDateTime, "IFD0 DateTime")
	p.dates(ifd0, "IFD0")
	p.xmp(ifd0)
	if _, ok := ifd0[tagExifIFD]; ok {
		off, ok := tr.first(ifd0, tagExifIFD)
		if !ok || off == 0 {
			return nil, nil, fmt.Errorf("unreadable Exif IFD pointer")
		}
		ex, _, err := tr.readIFD(off)
		if err != nil {
			return nil, nil, fmt.Errorf("exif IFD: %w", err)
		}
		p.dates(ex, "EXIF")
	}
	if p.err != nil {
		return nil, nil, p.err
	}
	return p.finish()
}

type skip struct {
	off int64
	msg string
}

type patcher struct {
	t        *tiffReader
	size     int64
	exifDate []byte // the target as "YYYY:MM:DD HH:MM:SS"
	when     time.Time
	patches  []Patch
	skips    []skip
	err      error
}

// dates handles the Exif-IFD date and sub-second tags in one IFD.
func (p *patcher) dates(entries map[uint16]ifdEntry, where string) {
	p.ascii(entries, tagDateTimeOriginal, where+" DateTimeOriginal")
	p.ascii(entries, tagDateTimeDigitized, where+" DateTimeDigitized")
	p.subsec(entries, tagSubSecTime, where+" SubSecTime")
	p.subsec(entries, tagSubSecOriginal, where+" SubSecTimeOriginal")
	p.subsec(entries, tagSubSecDigitized, where+" SubSecTimeDigitized")
}

// value returns the file offset of an entry's n value bytes: inline in the entry
// when they fit in 4 bytes, else at the offset the entry holds. ok is false (and a
// skip is recorded) when they lie outside the file.
func (p *patcher) value(e ifdEntry, n int64, what string) (int64, bool) {
	off := e.valueAt
	if n > 4 {
		off = int64(p.t.bo.Uint32(e.value[:]))
	}
	if off < 0 || off+n > p.size {
		p.skips = append(p.skips, skip{e.valueAt, what + ": value outside the file"})
		return 0, false
	}
	return off, true
}

func (p *patcher) read(off, n int64) ([]byte, bool) {
	b := make([]byte, n)
	// io.ReaderAt may return io.EOF with a full read that ends at the end of the input.
	if got, err := p.t.r.ReadAt(b, off); err != nil && !(err == io.EOF && got == len(b)) {
		if p.err == nil {
			p.err = fmt.Errorf("read at %d: %w", off, err)
		}
		return nil, false
	}
	return b, true
}

// ascii patches the first 19 bytes of an ASCII date-time tag.
func (p *patcher) ascii(entries map[uint16]ifdEntry, tag uint16, what string) {
	e, ok := entries[tag]
	if !ok {
		return
	}
	if e.typ != typeASCII || e.count < 19 {
		p.skips = append(p.skips, skip{e.valueAt, what + ": unparseable"})
		return
	}
	off, ok := p.value(e, int64(e.count), what)
	if !ok {
		return
	}
	old, ok := p.read(off, 19)
	if !ok {
		return
	}
	if !exifDate.Match(old) && !blankExifDate(old) {
		p.skips = append(p.skips, skip{off, what + ": unparseable"})
		return
	}
	p.add(off, old, p.exifDate, what)
}

// blankExifDate reports an unknown date: every byte a space or NUL, except that
// the date's colon positions may hold ':' (EXIF's "    :  :     :  :  ").
func blankExifDate(b []byte) bool {
	for i, c := range b {
		if c == ' ' || c == 0 || (c == ':' && (i == 4 || i == 7 || i == 13 || i == 16)) {
			continue
		}
		return false
	}
	return true
}

// subsec sets every digit of an ASCII sub-second tag to '0'.
func (p *patcher) subsec(entries map[uint16]ifdEntry, tag uint16, what string) {
	e, ok := entries[tag]
	if !ok {
		return
	}
	if e.typ != typeASCII || e.count == 0 || e.count > maxSubSec {
		p.skips = append(p.skips, skip{e.valueAt, what + ": unparseable"})
		return
	}
	off, ok := p.value(e, int64(e.count), what)
	if !ok {
		return
	}
	old, ok := p.read(off, int64(e.count))
	if !ok {
		return
	}
	nw := append([]byte(nil), old...)
	for i, c := range nw {
		if c >= '0' && c <= '9' {
			nw[i] = '0'
		}
	}
	p.add(off, old, nw, what)
}

// xmp rewrites the date values in IFD0's XMP packet.
func (p *patcher) xmp(entries map[uint16]ifdEntry) {
	e, ok := entries[tagXMLPacket]
	if !ok {
		return
	}
	switch {
	case e.typ != typeByte && e.typ != typeUndefined && e.typ != typeASCII:
		p.skips = append(p.skips, skip{e.valueAt, fmt.Sprintf("XMP: unsupported type %d", e.typ)})
		return
	case e.count > maxXMPPacket:
		p.skips = append(p.skips, skip{e.valueAt, "XMP: packet over 1 MiB, not searched"})
		return
	}
	base, ok := p.value(e, int64(e.count), "XMP")
	if !ok {
		return
	}
	pkt, ok := p.read(base, int64(e.count))
	if !ok {
		return
	}
	if body := bytes.TrimRight(pkt, "\x00"); bytes.HasPrefix(body, []byte{0xFE, 0xFF}) ||
		bytes.HasPrefix(body, []byte{0xFF, 0xFE}) || bytes.IndexByte(body, 0) >= 0 {
		p.skips = append(p.skips, skip{base, "XMP: packet is not UTF-8, left unchanged"})
		return
	}
	// Matching runs on a copy with comments blanked out (same offsets); values are
	// taken from the packet itself, which equals the copy outside comments.
	masked := maskXMLComments(pkt)
	var consumed [][2]int // spans matched as a property, rewritten or reported
	for _, prop := range xmpDateProps {
		for form, re := range xmpPropRes[prop] {
			for _, m := range re.FindAllSubmatchIndex(masked, -1) {
				if form == 0 && !inStartTag(masked, m[0]) {
					continue // attribute-looking text in element content
				}
				consumed = append(consumed, [2]int{m[0], m[1]})
				vs, ve := m[2], m[3]
				if vs < 0 { // the attribute's single-quoted alternative
					vs, ve = m[4], m[5]
				}
				for vs < ve && isXMLSpace(pkt[vs]) {
					vs++
				}
				for ve > vs && isXMLSpace(pkt[ve-1]) {
					ve--
				}
				what := "XMP " + prop
				old := pkt[vs:ve:ve]
				nw, ok := p.isoValue(old)
				switch {
				case !ok:
					p.skips = append(p.skips, skip{base + int64(vs), what + ": unparseable"})
				case len(nw) != len(old):
					p.skips = append(p.skips, skip{base + int64(vs), what + ": would change length"})
				default:
					p.add(base+int64(vs), append([]byte(nil), old...), nw, what)
				}
			}
		}
	}
	// Report every other occurrence of a date name once.
	reported := map[string]bool{}
	for _, m := range xmpDateName.FindAllIndex(masked, -1) {
		in := false
		for _, c := range consumed {
			if m[0] >= c[0] && m[0] < c[1] {
				in = true
				break
			}
		}
		name := string(masked[m[0]:m[1]])
		if in || reported[name] {
			continue
		}
		reported[name] = true
		p.skips = append(p.skips, skip{base + int64(m[0]), "XMP " + name + ": not in a form cull rewrites, left unchanged"})
	}
}

// maskXMLComments returns a copy of b with every <!-- ... --> (an unterminated
// one to the end) replaced by spaces, so offsets are unchanged.
func maskXMLComments(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := 0; ; {
		s := bytes.Index(out[i:], []byte("<!--"))
		if s < 0 {
			return out
		}
		s += i
		e := bytes.Index(out[s+4:], []byte("-->"))
		end := len(out)
		if e >= 0 {
			end = s + 4 + e + 3
		}
		for j := s; j < end; j++ {
			out[j] = ' '
		}
		i = end
	}
}

// inStartTag reports whether position i lies inside a start tag: after a '<' that
// opens an element (not "</", "<!" or "<?") with no '>' between them.
func inStartTag(b []byte, i int) bool {
	lt := bytes.LastIndexByte(b[:i], '<')
	if lt < 0 || bytes.LastIndexByte(b[:i], '>') > lt || lt+1 >= len(b) {
		return false
	}
	switch b[lt+1] {
	case '/', '!', '?':
		return false
	}
	return true
}

func isXMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isoValue rewrites an ISO 8601 value digit for digit: the date becomes the
// target's, HH:MM and SS (where present) its time, fraction digits '0'; the zone
// suffix and the value's shape are kept.
func (p *patcher) isoValue(v []byte) ([]byte, bool) {
	m := isoDate.FindSubmatchIndex(v)
	if m == nil {
		return nil, false
	}
	out := append([]byte(nil), v...)
	put := func(g int, s string) {
		if m[2*g] >= 0 {
			copy(out[m[2*g]:m[2*g+1]], s)
		}
	}
	put(1, p.when.Format("2006-01-02"))
	put(2, p.when.Format("15:04"))
	put(3, p.when.Format("05"))
	if m[8] >= 0 {
		for i := m[8]; i < m[9]; i++ {
			out[i] = '0'
		}
	}
	return out, true
}

// add records a patch unless the bytes already hold the target.
func (p *patcher) add(off int64, old, nw []byte, what string) {
	if len(old) != len(nw) {
		if p.err == nil {
			p.err = fmt.Errorf("%s: patch would change length (%d → %d bytes)", what, len(old), len(nw))
		}
		return
	}
	if bytes.Equal(old, nw) {
		return
	}
	p.patches = append(p.patches, Patch{Off: off, Old: old, New: append([]byte(nil), nw...), What: what})
}

// finish sorts the patches, merges identical ones (two tags sharing one stored
// value), and refuses overlaps, which would be a bug.
func (p *patcher) finish() ([]Patch, []string, error) {
	sort.SliceStable(p.patches, func(i, j int) bool { return p.patches[i].Off < p.patches[j].Off })
	var out []Patch
	for _, pt := range p.patches {
		if n := len(out); n > 0 {
			prev := &out[n-1]
			if prev.Off == pt.Off && bytes.Equal(prev.Old, pt.Old) && bytes.Equal(prev.New, pt.New) {
				prev.What += ", " + pt.What
				continue
			}
			if prev.Off+int64(len(prev.New)) > pt.Off {
				return nil, nil, fmt.Errorf("overlapping patches: %s at %d and %s at %d", prev.What, prev.Off, pt.What, pt.Off)
			}
		}
		out = append(out, pt)
	}
	sort.SliceStable(p.skips, func(i, j int) bool { return p.skips[i].off < p.skips[j].off })
	var skipped []string
	for _, s := range p.skips {
		skipped = append(skipped, s.msg)
	}
	return out, skipped, nil
}

// ContentCredentials reports whether the DNG read from r (size bytes) carries a
// C2PA Content Credentials manifest (IFD0 tag 0xCD41). Such a frame's signed
// manifest covers its capture dates, so cull never patches it.
func ContentCredentials(r io.ReaderAt, size int64) (bool, error) {
	tr, ifd0Off, err := newTIFFReader(r, size)
	if err != nil {
		return false, err
	}
	ifd0, _, err := tr.readIFD(ifd0Off)
	if err != nil {
		return false, err
	}
	_, ok := ifd0[tagC2PA]
	return ok, nil
}
