// Package dngtest builds small synthetic DNG (TIFF) files for tests that need real
// capture-date fields to read or patch: dng itself, offload, redate and rename. It is
// a test helper only; nothing outside _test.go files should import it.
//
// A built file is a valid TIFF that dng.ReadExif and dng.ReadExifFrom can read. Its
// layout, in order:
//
//	header (8 bytes)
//	filler (16)
//	IFD0: NewSubfileType 0 (1 with a Preview), [Compression, StripOffsets,
//	  StripByteCounts: the Preview], [DateTime], [XMLPacket], [ExifIFD pointer],
//	  [C2PA 0xCD41]
//	filler (16)
//	[Exif IFD: DateTimeOriginal, DateTimeDigitized, OffsetTime, SubSecTime, SubSecTimeOriginal,
//	  SubSecTimeDigitized]
//	filler (16)
//	out-of-line values, in entry order, each followed by filler (8, plus 1 to keep
//	  the next value on an even offset when needed)
//	[Preview, then filler (16)]
//	Payload, then filler (16)
//
// Filler is a fixed non-ASCII pattern (0xA5 0x5A 0xC3 0x3C, repeating), so a test
// can check that every byte outside the patched ranges is unchanged and that no
// date-like text is found there by accident.
package dngtest

import (
	"encoding/binary"
	"testing"

	"github.com/jefflaplante/cull/internal/dng"
)

// Fixture describes one synthetic DNG. Every string field is optional: "" omits
// that tag. The Exif IFD (and IFD0's pointer to it) is written only when at least
// one of DTO, DTD, OffsetTime, SubSec, SubSecOrig or SubSecDTD is set, so a fixture with none
// of them has no Exif IFD at all.
type Fixture struct {
	// DateTime is IFD0's 0x0132; DTO and DTD are the Exif IFD's 0x9003 and 0x9004.
	// They are meant to be 19-character "YYYY:MM:DD HH:MM:SS" values but are
	// written as given (plus a NUL), so a test can store a malformed one.
	DateTime, DTO, DTD string
	// SubSec is 0x9290 SubSecTime, SubSecOrig 0x9291 SubSecTimeOriginal and
	// SubSecDTD 0x9292 SubSecTimeDigitized (ASCII, NUL-terminated; up to 3
	// characters fit inline in the entry, longer values go out of line).
	SubSec, SubSecOrig, SubSecDTD string
	// XMP is the full packet stored in IFD0's 0x02BC XMLPacket (type BYTE, no NUL).
	XMP string
	// BigEndian writes an "MM" file; the default is little-endian "II".
	BigEndian bool
	// OffsetTime is the Exif IFD's 0x9010 (e.g. "+01:00"). Date fixing must leave
	// it alone.
	OffsetTime string
	// C2PA, when non-nil, is stored as IFD0's 0xCD41 (UNDEFINED), the tag that
	// holds a C2PA Content Credentials manifest (the M11-P's L… frames have one).
	C2PA []byte
	// Payload stands in for raw image data: it is written after the values, so two
	// fixtures with the same dates can still differ in content (and hash).
	Payload []byte
	// Preview, when non-nil, is a baseline JPEG stored as IFD0's strip, with IFD0
	// marked reduced-resolution (NewSubfileType 1) and JPEG-compressed: what
	// dng.Extract finds as the embedded preview, so the pipeline can judge the file.
	Preview []byte
	// Make and Model are IFD0's 0x010F and 0x0110 (e.g. "Leica Camera AG",
	// "LEICA M10-R"): what picks a camera's develop preset.
	Make, Model string
}

const (
	tagNewSubfileType = 0x00FE
	tagCompression    = 0x0103
	tagMake           = 0x010F
	tagModel          = 0x0110
	tagStripOffsets   = 0x0111
	tagStripByteCount = 0x0117
	tagDateTime       = 0x0132
	tagXMLPacket      = 0x02BC
	tagExifIFD        = 0x8769
	tagDTO            = 0x9003
	tagDTD            = 0x9004
	tagOffsetTime     = 0x9010
	tagSubSec         = 0x9290
	tagSubSecOrig     = 0x9291
	tagSubSecDTD      = 0x9292
	tagC2PA           = 0xCD41

	typeByte  = 1
	typeASCII = 2
	typeShort = 3
	typeLong  = 4
	typeUndef = 7
)

var fillerPattern = [4]byte{0xA5, 0x5A, 0xC3, 0x3C}

func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fillerPattern[i%4]
	}
	return b
}

type entry struct {
	tag, typ uint16
	count    uint32
	raw      []byte // the value's bytes; inline when len(raw) <= 4
	off      uint32 // assigned offset of an out-of-line value
}

func ascii(tag uint16, s string) entry {
	raw := append([]byte(s), 0)
	return entry{tag: tag, typ: typeASCII, count: uint32(len(raw)), raw: raw}
}

// Build returns the bytes of the DNG f describes. Entries are in tag order, and
// ASCII values longer than 4 bytes go out of line after the IFDs.
func Build(tb testing.TB, f Fixture) []byte {
	tb.Helper()
	var bo interface {
		binary.ByteOrder
		binary.AppendByteOrder
	} = binary.LittleEndian
	if f.BigEndian {
		bo = binary.BigEndian
	}

	var exif []entry
	if f.DTO != "" {
		exif = append(exif, ascii(tagDTO, f.DTO))
	}
	if f.DTD != "" {
		exif = append(exif, ascii(tagDTD, f.DTD))
	}
	if f.OffsetTime != "" {
		exif = append(exif, ascii(tagOffsetTime, f.OffsetTime))
	}
	if f.SubSec != "" {
		exif = append(exif, ascii(tagSubSec, f.SubSec))
	}
	if f.SubSecOrig != "" {
		exif = append(exif, ascii(tagSubSecOrig, f.SubSecOrig))
	}
	if f.SubSecDTD != "" {
		exif = append(exif, ascii(tagSubSecDTD, f.SubSecDTD))
	}

	ifd0 := []entry{{tag: tagNewSubfileType, typ: typeLong, count: 1, raw: make([]byte, 4)}}
	camera := func() { // 0x010F, 0x0110: after Compression, before StripOffsets
		if f.Make != "" {
			ifd0 = append(ifd0, ascii(tagMake, f.Make))
		}
		if f.Model != "" {
			ifd0 = append(ifd0, ascii(tagModel, f.Model))
		}
	}
	stripAt := -1
	if f.Preview == nil {
		camera()
	} else {
		bo.PutUint32(ifd0[0].raw, 1) // reduced resolution: a preview
		comp := make([]byte, 4)
		bo.PutUint16(comp, 7) // JPEG
		ifd0 = append(ifd0, entry{tag: tagCompression, typ: typeShort, count: 1, raw: comp})
		camera()
		stripAt = len(ifd0)
		ifd0 = append(ifd0, entry{tag: tagStripOffsets, typ: typeLong, count: 1, raw: make([]byte, 4)})
		n := make([]byte, 4)
		bo.PutUint32(n, uint32(len(f.Preview)))
		ifd0 = append(ifd0, entry{tag: tagStripByteCount, typ: typeLong, count: 1, raw: n})
	}
	if f.DateTime != "" {
		ifd0 = append(ifd0, ascii(tagDateTime, f.DateTime))
	}
	if f.XMP != "" {
		ifd0 = append(ifd0, entry{tag: tagXMLPacket, typ: typeByte, count: uint32(len(f.XMP)), raw: []byte(f.XMP)})
	}
	exifPtr := -1
	if len(exif) > 0 {
		exifPtr = len(ifd0)
		ifd0 = append(ifd0, entry{tag: tagExifIFD, typ: typeLong, count: 1, raw: make([]byte, 4)})
	}
	if f.C2PA != nil {
		ifd0 = append(ifd0, entry{tag: tagC2PA, typ: typeUndef, count: uint32(len(f.C2PA)), raw: f.C2PA})
	}

	ifdSize := func(es []entry) uint32 { return 2 + uint32(len(es))*12 + 4 }
	const gap = 16
	ifd0Off := uint32(8 + gap)
	exifOff := ifd0Off + ifdSize(ifd0) + gap
	dataOff := exifOff
	if len(exif) > 0 {
		dataOff += ifdSize(exif) + gap
	}
	if exifPtr >= 0 {
		bo.PutUint32(ifd0[exifPtr].raw, exifOff)
	}

	// Lay out the out-of-line values, each followed by filler, on even offsets.
	var data []byte
	place := func(es []entry) {
		for i := range es {
			if len(es[i].raw) <= 4 {
				continue
			}
			es[i].off = dataOff + uint32(len(data))
			data = append(data, es[i].raw...)
			pad := 8
			if (len(data)+pad)%2 == 1 {
				pad++
			}
			data = append(data, filler(pad)...)
		}
	}
	place(ifd0)
	place(exif)
	if stripAt >= 0 {
		bo.PutUint32(ifd0[stripAt].raw, dataOff+uint32(len(data)))
		data = append(data, f.Preview...)
		data = append(data, filler(gap)...)
	}

	writeIFD := func(out []byte, es []entry) []byte {
		out = bo.AppendUint16(out, uint16(len(es)))
		for _, e := range es {
			out = bo.AppendUint16(out, e.tag)
			out = bo.AppendUint16(out, e.typ)
			out = bo.AppendUint32(out, e.count)
			if len(e.raw) <= 4 {
				var v [4]byte
				copy(v[:], e.raw)
				out = append(out, v[:]...)
			} else {
				out = bo.AppendUint32(out, e.off)
			}
		}
		return bo.AppendUint32(out, 0) // no next IFD
	}

	var out []byte
	if f.BigEndian {
		out = append(out, 'M', 'M')
	} else {
		out = append(out, 'I', 'I')
	}
	out = bo.AppendUint16(out, 42)
	out = bo.AppendUint32(out, ifd0Off)
	out = append(out, filler(gap)...)
	out = writeIFD(out, ifd0)
	out = append(out, filler(gap)...)
	if len(exif) > 0 {
		out = writeIFD(out, exif)
		out = append(out, filler(gap)...)
	}
	if uint32(len(out)) != dataOff {
		tb.Fatalf("dngtest: layout error: data at %d, want %d", len(out), dataOff)
	}
	out = append(out, data...)
	out = append(out, f.Payload...)
	return append(out, filler(gap)...)
}

// Apply returns a copy of b with every patch's New bytes written at its Off. It
// does not check Old: tests compare that themselves.
func Apply(b []byte, ps []dng.Patch) []byte {
	out := append([]byte(nil), b...)
	for _, p := range ps {
		copy(out[p.Off:], p.New)
	}
	return out
}
