package dng_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
)

var target = time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)

// checkPatches asserts the invariants every caller relies on: sorted,
// non-overlapping, same length, Old equal to the file's bytes, and no byte outside
// a patch changed by applying them.
func checkPatches(t *testing.T, b []byte, ps []dng.Patch) []byte {
	t.Helper()
	for i, p := range ps {
		if len(p.Old) != len(p.New) || p.Off < 0 || p.Off+int64(len(p.Old)) > int64(len(b)) ||
			!bytes.Equal(b[p.Off:p.Off+int64(len(p.Old))], p.Old) {
			t.Fatalf("patch %d (%s) inconsistent", i, p.What)
		}
		if bytes.Equal(p.Old, p.New) {
			t.Fatalf("patch %d (%s) changes nothing", i, p.What)
		}
		if i > 0 && ps[i-1].Off+int64(len(ps[i-1].New)) > p.Off {
			t.Fatal("overlap or unsorted")
		}
	}
	got := dngtest.Apply(b, ps)
	if len(got) != len(b) {
		t.Fatal("length changed")
	}
	in := func(i int) bool {
		for _, p := range ps {
			if int64(i) >= p.Off && int64(i) < p.Off+int64(len(p.New)) {
				return true
			}
		}
		return false
	}
	for i := range b {
		if b[i] != got[i] && !in(i) {
			t.Fatalf("byte %d changed outside a patch", i)
		}
	}
	return got
}

func whats(ps []dng.Patch) []string {
	var w []string
	for _, p := range ps {
		w = append(w, p.What)
	}
	return w
}

func TestPatchDatesSetsEveryField(t *testing.T) {
	xmpPkt := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmlns:exif="http://ns.adobe.com/exif/1.0/" xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" ` +
		`xmp:CreateDate="2025-12-28T00:05:59.53" xmp:ModifyDate="2025-12-28T00:05:59+01:00">` +
		`<exif:DateTimeOriginal>2025-12-28T00:05:59.530+01:00</exif:DateTimeOriginal>` +
		`<photoshop:DateCreated>2025-12-28</photoshop:DateCreated>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", DTD: "2025:12:28 00:05:59",
		SubSec: "530", SubSecOrig: "53", XMP: xmpPkt, OffsetTime: "+01:00"})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err %v skipped %v", err, skipped)
	}
	got := checkPatches(t, b, ps)
	for _, want := range []string{"2026:10:04 12:00:00", `xmp:CreateDate="2026-10-04T12:00:00.00"`,
		`xmp:ModifyDate="2026-10-04T12:00:00+01:00"`, `<exif:DateTimeOriginal>2026-10-04T12:00:00.000+01:00</exif:DateTimeOriginal>`,
		`<photoshop:DateCreated>2026-10-04</photoshop:DateCreated>`, "+01:00\x00"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}
	if bytes.Contains(got, []byte("2025:12:28")) || bytes.Contains(got, []byte("2025-12-28")) {
		t.Error("an old date survived")
	}
	wantWhats := []string{"IFD0 DateTime", "XMP xmp:CreateDate", "XMP xmp:ModifyDate", "XMP exif:DateTimeOriginal",
		"XMP photoshop:DateCreated", "EXIF DateTimeOriginal", "EXIF DateTimeDigitized", "EXIF SubSecTime", "EXIF SubSecTimeOriginal"}
	if w := whats(ps); len(w) != len(wantWhats) {
		t.Fatalf("patches %v, want %v (any order)", w, wantWhats)
	}
	for _, w := range wantWhats {
		if !strings.Contains(strings.Join(whats(ps), "|")+"|", w+"|") {
			t.Errorf("no patch for %s (got %v)", w, whats(ps))
		}
	}
	ex, err := dng.ReadExifFrom(bytes.NewReader(got), int64(len(got)))
	if err != nil || ex.DateTimeOriginal != "2026:10:04 12:00:00" || ex.SubSec != "00" {
		t.Fatalf("read back %+v %v", ex, err)
	}
	// Idempotent: a patched file needs no patches.
	if again, skipped, err := dng.PatchDates(bytes.NewReader(got), int64(len(got)), target); len(again) != 0 || len(skipped) != 0 || err != nil {
		t.Fatalf("second pass returned %v, skipped %v, err %v", whats(again), skipped, err)
	}
}

// Each ASCII patch sits exactly on the value's first 19 bytes; the NUL after it is
// not part of the patch.
func TestPatchDatesExactOffsets(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		f := dngtest.Fixture{DateTime: "2025:01:01 01:01:01", DTO: "2025:02:02 02:02:02", DTD: "2025:03:03 03:03:03",
			SubSec: "530", SubSecOrig: "5", BigEndian: bigEndian}
		b := dngtest.Build(t, f)
		ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
		if err != nil {
			t.Fatal(err)
		}
		checkPatches(t, b, ps)
		want := map[string]struct {
			old string
			n   int
		}{
			"IFD0 DateTime":           {f.DateTime, 19},
			"EXIF DateTimeOriginal":   {f.DTO, 19},
			"EXIF DateTimeDigitized":  {f.DTD, 19},
			"EXIF SubSecTime":         {"530\x00", 4}, // inline, count 4
			"EXIF SubSecTimeOriginal": {"5\x00", 2},   // inline, count 2
		}
		if len(ps) != len(want) {
			t.Fatalf("patches %v", whats(ps))
		}
		for _, p := range ps {
			w, ok := want[p.What]
			if !ok {
				t.Fatalf("unexpected patch %s", p.What)
			}
			if off := bytes.Index(b, []byte(w.old)); int64(off) != p.Off || len(p.Old) != w.n {
				t.Errorf("%s (BE %v): off %d len %d, want off %d len %d", p.What, bigEndian, p.Off, len(p.Old), off, w.n)
			}
		}
	}
}

func TestPatchDatesSubSecZeroed(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", SubSec: "530", SubSecOrig: "53"})
	ps, _, _ := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	got := checkPatches(t, b, ps)
	ex, _ := dng.ReadExifFrom(bytes.NewReader(got), int64(len(got)))
	if ex.SubSec != "00" {
		t.Fatalf("subsec %q", ex.SubSec)
	}
}

func TestPatchDatesBigEndianAndMissingTags(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", BigEndian: true}) // no DateTime, no DTD, no XMP
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 1 || ps[0].What != "EXIF DateTimeOriginal" {
		t.Fatalf("%v %+v", err, ps)
	}
	checkPatches(t, b, ps)
}

func TestPatchDatesNoExifIFD(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59"})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 || len(ps) != 1 || ps[0].What != "IFD0 DateTime" {
		t.Fatalf("err %v skipped %v patches %v", err, skipped, whats(ps))
	}
	checkPatches(t, b, ps)
}

func TestPatchDatesSkipsUnparseable(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DTO: "not a date at all!!", XMP: `<x:xmpmeta><rdf:Description xmp:CreateDate="yesterday"/></x:xmpmeta>`})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 0 || len(skipped) != 2 {
		t.Fatalf("err %v patches %d skipped %v", err, len(ps), skipped)
	}
	for _, s := range skipped {
		if !strings.HasSuffix(s, ": unparseable") {
			t.Errorf("skipped %q", s)
		}
	}
}

// A field that already holds the target is left out; the rest are still patched.
func TestPatchDatesOmitsFieldsAlreadyAtTarget(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2026:10:04 12:00:00", DTO: "2025:12:28 00:05:59", SubSecOrig: "00",
		XMP: `<rdf:Description xmp:CreateDate="2026-10-04T12:00:00.00Z" xmp:ModifyDate="2025-12-28T00:05"/>`})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err %v skipped %v", err, skipped)
	}
	if w := strings.Join(whats(ps), ","); w != "XMP xmp:ModifyDate,EXIF DateTimeOriginal" {
		t.Fatalf("patches %s", w)
	}
	got := checkPatches(t, b, ps)
	if !bytes.Contains(got, []byte(`xmp:ModifyDate="2026-10-04T12:00"`)) {
		t.Errorf("HH:MM value not rewritten in its own format: %s", got)
	}
}

// Blank ASCII dates are filled in: all spaces, all NULs, or EXIF's "unknown" form
// (blanks with the colons kept).
func TestPatchDatesFillsBlankDates(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: strings.Repeat(" ", 19), DTO: "    :  :     :  :  ",
		DTD: strings.Repeat("\x00", 19)})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 || len(ps) != 3 {
		t.Fatalf("err %v patches %v skipped %v", err, whats(ps), skipped)
	}
	got := checkPatches(t, b, ps)
	if n := bytes.Count(got, []byte("2026:10:04 12:00:00\x00")); n != 3 {
		t.Errorf("%d blank dates filled, want 3", n)
	}
}

// A date-shaped field with a stray character is unparseable: reported, not patched.
func TestPatchDatesSkipsNearDates(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28T00:05:59", DTO: "2025:12:28 00:05:5", DTD: "    :  :  x  :  :  "})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 0 || strings.Join(skipped, "|") !=
		"IFD0 DateTime: unparseable|EXIF DateTimeOriginal: unparseable|EXIF DateTimeDigitized: unparseable" {
		t.Fatalf("err %v patches %v skipped %v", err, whats(ps), skipped)
	}
}

// XMP in both quote styles, element form with attributes and whitespace, every
// listed property, and values that are reported rather than rewritten.
func TestPatchDatesXMPForms(t *testing.T) {
	xmp := `<rdf:Description xmp:MetadataDate='2025-12-28T00:05:59.123456-08:00' ` + "\n\t" +
		`exif:DateTimeDigitized = "2025-12-28T00:05Z" xmp:CreateDateX="2025-12-28">` +
		`<exif:DateTimeOriginal rdf:parseType="Literal"> 2025-12-28T00:05:59 </exif:DateTimeOriginal>` +
		`<photoshop:DateCreated>2025-12-28T00:05:59+0100</photoshop:DateCreated>` +
		`<xmp:ModifyDate></xmp:ModifyDate>` +
		`</rdf:Description>`
	b := dngtest.Build(t, dngtest.Fixture{XMP: xmp, BigEndian: true})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil {
		t.Fatal(err)
	}
	got := checkPatches(t, b, ps)
	for _, want := range []string{`xmp:MetadataDate='2026-10-04T12:00:00.000000-08:00'`,
		`exif:DateTimeDigitized = "2026-10-04T12:00Z"`, `xmp:CreateDateX="2025-12-28"`,
		`<exif:DateTimeOriginal rdf:parseType="Literal"> 2026-10-04T12:00:00 </exif:DateTimeOriginal>`,
		`<photoshop:DateCreated>2025-12-28T00:05:59+0100</photoshop:DateCreated>`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	// +0100 is not the ISO 8601 extended zone the spec allows; the empty
	// ModifyDate isn't a date either. Both are reported and left alone.
	if strings.Join(skipped, "|") != "XMP photoshop:DateCreated: unparseable|XMP xmp:ModifyDate: unparseable" {
		t.Errorf("skipped %v", skipped)
	}
}

// A target that can't be written at the same length is refused outright.
func TestPatchDatesRefusesLengthChange(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59"})
	for _, y := range []int{10000, -1} {
		ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC))
		if err == nil || len(ps) != 0 {
			t.Errorf("year %d: err %v patches %d", y, err, len(ps))
		}
	}
}

// Two tags sharing one stored value get one patch, not an overlap error.
func TestPatchDatesSharedValueStorage(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:01:01 01:01:01", DTO: "2025:12:28 00:05:59"})
	// Point IFD0's DateTime (entry 1, after NewSubfileType) at DTO's bytes.
	ifd0 := binary.LittleEndian.Uint32(b[4:8])
	dto := bytes.Index(b, []byte("2025:12:28 00:05:59"))
	binary.LittleEndian.PutUint32(b[ifd0+2+12+8:], uint32(dto))
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 1 || ps[0].Off != int64(dto) {
		t.Fatalf("err %v patches %+v", err, ps)
	}
	if ps[0].What != "IFD0 DateTime, EXIF DateTimeOriginal" {
		t.Errorf("what %q", ps[0].What)
	}
}

// Sub-second digits are zeroed; anything else in the value stays.
func TestPatchDatesSubSecKeepsNonDigits(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{SubSec: "12 ", SubSecOrig: "000"})
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 1 || ps[0].What != "EXIF SubSecTime" || string(ps[0].New) != "00 \x00" {
		t.Fatalf("err %v patches %+v", err, ps)
	}
}

func TestPatchDatesRejectsNonTIFF(t *testing.T) {
	b := []byte("not a tiff file at all")
	if _, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target); err == nil {
		t.Fatal("want an error")
	}
}

// Payload makes fixtures with equal dates differ, and the payload is never patched.
func TestBuildPayload(t *testing.T) {
	a := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", Payload: []byte("frame-a")})
	b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", Payload: []byte("frame-b")})
	if bytes.Equal(a, b) {
		t.Fatal("payloads didn't make the files differ")
	}
	ex, err := dng.ReadExifFrom(bytes.NewReader(a), int64(len(a)))
	if err != nil || ex.DateTimeOriginal != "2025:12:28 00:05:59" {
		t.Fatalf("%+v %v", ex, err)
	}
}

// Some writers put DateTimeOriginal in IFD0 (ReadExif reads it there); it is a
// capture date, so it is patched too.
func TestPatchDatesDateTimeOriginalInIFD0(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59"})
	// Re-tag IFD0's entry 1 (DateTime) as 0x9003; it is still the last entry.
	ifd0 := binary.LittleEndian.Uint32(b[4:8])
	binary.LittleEndian.PutUint16(b[ifd0+2+12:], 0x9003)
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 || len(ps) != 1 || ps[0].What != "IFD0 DateTimeOriginal" {
		t.Fatalf("err %v skipped %v patches %v", err, skipped, whats(ps))
	}
	checkPatches(t, b, ps)
}

func TestContentCredentials(t *testing.T) {
	with := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", C2PA: []byte("jumb c2pa manifest stand-in")})
	without := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59"})
	if ok, err := dng.ContentCredentials(bytes.NewReader(with), int64(len(with))); !ok || err != nil {
		t.Errorf("with C2PA: %v %v", ok, err)
	}
	if ok, err := dng.ContentCredentials(bytes.NewReader(without), int64(len(without))); ok || err != nil {
		t.Errorf("without C2PA: %v %v", ok, err)
	}
	junk := []byte("not a tiff")
	if _, err := dng.ContentCredentials(bytes.NewReader(junk), int64(len(junk))); err == nil {
		t.Error("non-TIFF: want an error")
	}
}

// A frame with signed Content Credentials is never byte-patched: its manifest
// repeats the dates and its hash binding covers them.
func TestPatchDatesLeavesContentCredentialsFrames(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", DTD: "2025:12:28 00:05:59",
		SubSec: "530", XMP: `<rdf:Description xmp:CreateDate="2025-12-28T00:05:59"/>`, C2PA: bytes.Repeat([]byte{0x6A}, 300)})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 0 || len(skipped) != 1 || skipped[0] != "C2PA Content Credentials: left unchanged so its signature stays valid (it carries its own signed capture dates)" {
		t.Fatalf("err %v patches %v skipped %q", err, whats(ps), skipped)
	}
}

// Attribute-looking text outside a start tag (element text, comments) is not a
// property: never patched. Text in element content is reported; comments are ignored.
func TestPatchDatesXMPAttributesOnlyInStartTags(t *testing.T) {
	xmp := `<rdf:Description><dc:description><rdf:Alt><rdf:li xml:lang="x-default">see xmp:CreateDate="2025-12-28T00:05:59"</rdf:li></rdf:Alt></dc:description>` +
		`<!-- xmp:ModifyDate="2025-12-28T00:05:59" <exif:DateTimeOriginal>2025-12-28</exif:DateTimeOriginal> -->` +
		`</rdf:Description>`
	b := dngtest.Build(t, dngtest.Fixture{XMP: xmp})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 0 {
		t.Fatalf("err %v patches %v", err, whats(ps))
	}
	if strings.Join(skipped, "|") != "XMP xmp:CreateDate: not in a form cull rewrites, left unchanged" {
		t.Errorf("skipped %q", skipped)
	}
}

// Date properties cull doesn't rewrite are reported, never silently missed: another
// prefix (xap:), or a value wrapped in rdf:Seq.
func TestPatchDatesReportsUnmatchedDateNames(t *testing.T) {
	xmp := `<rdf:Description xap:CreateDate="2025-12-28T00:05:59" xmp:ModifyDate="2025-12-28T00:05:59">` +
		`<photoshop:DateCreated><rdf:Seq><rdf:li>2025-12-28</rdf:li></rdf:Seq></photoshop:DateCreated>` +
		`</rdf:Description>`
	b := dngtest.Build(t, dngtest.Fixture{XMP: xmp})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 1 || ps[0].What != "XMP xmp:ModifyDate" {
		t.Fatalf("err %v patches %v", err, whats(ps))
	}
	checkPatches(t, b, ps)
	want := "XMP xap:CreateDate: not in a form cull rewrites, left unchanged|" +
		"XMP photoshop:DateCreated: not in a form cull rewrites, left unchanged"
	if strings.Join(skipped, "|") != want {
		t.Errorf("skipped %q", skipped)
	}
}

func TestPatchDatesReportsNonUTF8Packet(t *testing.T) {
	utf16 := func(s string) string {
		out := []byte{0xFF, 0xFE}
		for _, c := range []byte(s) {
			out = append(out, c, 0)
		}
		return string(out)
	}
	for name, xmp := range map[string]string{
		"utf-16 BOM": utf16(`<rdf:Description xmp:CreateDate="2025-12-28T00:05:59"/>`),
		"NUL inside": "<rdf:Description xmp:CreateDate=\"2025-12-28T00:05:59\"/>\x00<x/>",
	} {
		b := dngtest.Build(t, dngtest.Fixture{XMP: xmp})
		ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
		if err != nil || len(ps) != 0 || len(skipped) != 1 || skipped[0] != "XMP: packet is not UTF-8, left unchanged" {
			t.Errorf("%s: err %v patches %v skipped %q", name, err, whats(ps), skipped)
		}
	}
	// Trailing NULs are padding, not a sign of UTF-16.
	b := dngtest.Build(t, dngtest.Fixture{XMP: "<rdf:Description xmp:CreateDate=\"2025-12-28T00:05:59\"/>\x00\x00"})
	if ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target); err != nil || len(ps) != 1 || len(skipped) != 0 {
		t.Errorf("NUL padding: err %v patches %v skipped %q", err, whats(ps), skipped)
	}
}

// eofAtEnd returns io.EOF with a full read that ends exactly at the end of the
// data, as the io.ReaderAt contract allows.
type eofAtEnd struct{ b []byte }

func (r eofAtEnd) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(r.b).ReadAt(p, off)
	if err == nil && off+int64(n) == int64(len(r.b)) {
		err = io.EOF
	}
	return n, err
}

func TestPatchDatesAcceptsEOFWithFullRead(t *testing.T) {
	xmp := `<rdf:Description xmp:CreateDate="2025-12-28T00:05:59"/>`
	b := dngtest.Build(t, dngtest.Fixture{XMP: xmp})
	b = b[:bytes.Index(b, []byte(xmp))+len(xmp)] // the packet is the file's last bytes
	ps, skipped, err := dng.PatchDates(eofAtEnd{b}, int64(len(b)), target)
	if err != nil || len(ps) != 1 || len(skipped) != 0 {
		t.Fatalf("err %v patches %v skipped %q", err, whats(ps), skipped)
	}
}

func TestPatchDatesSubSecDigitizedAndOutOfLine(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{SubSec: "53012", SubSecDTD: "7"})
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 || len(ps) != 2 {
		t.Fatalf("err %v patches %v skipped %q", err, whats(ps), skipped)
	}
	got := checkPatches(t, b, ps)
	for _, p := range ps {
		switch p.What {
		case "EXIF SubSecTime": // 6 bytes, out of line
			if string(p.New) != "00000\x00" || p.Off != int64(bytes.Index(b, []byte("53012"))) {
				t.Errorf("SubSecTime %+v", p)
			}
		case "EXIF SubSecTimeDigitized": // inline
			if string(p.New) != "0\x00" {
				t.Errorf("SubSecTimeDigitized %+v", p)
			}
		default:
			t.Errorf("unexpected %s", p.What)
		}
	}
	if !bytes.Contains(got, []byte("00000\x00")) {
		t.Error("out-of-line sub-seconds not zeroed")
	}
}

// exifIFD returns the Exif IFD's offset in a little-endian fixture whose IFD0 is
// NewSubfileType followed by the Exif pointer.
func exifIFD(b []byte) uint32 {
	ifd0 := binary.LittleEndian.Uint32(b[4:8])
	return binary.LittleEndian.Uint32(b[ifd0+2+12+8:])
}

func TestPatchDatesValuePastEOF(t *testing.T) {
	b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59", DTD: "2025:12:28 00:05:59"})
	binary.LittleEndian.PutUint32(b[exifIFD(b)+2+8:], uint32(len(b))-5) // DTO's value runs past EOF
	ps, skipped, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(ps) != 1 || ps[0].What != "EXIF DateTimeDigitized" ||
		strings.Join(skipped, "|") != "EXIF DateTimeOriginal: value outside the file" {
		t.Fatalf("err %v patches %v skipped %q", err, whats(ps), skipped)
	}
}

func TestPatchDatesUnreadableExifPointer(t *testing.T) {
	for _, ptr := range []uint32{0, 1 << 30} {
		b := dngtest.Build(t, dngtest.Fixture{DTO: "2025:12:28 00:05:59"})
		ifd0 := binary.LittleEndian.Uint32(b[4:8])
		binary.LittleEndian.PutUint32(b[ifd0+2+12+8:], ptr)
		if ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target); err == nil || len(ps) != 0 {
			t.Errorf("pointer %d: err %v patches %d", ptr, err, len(ps))
		}
	}
}
