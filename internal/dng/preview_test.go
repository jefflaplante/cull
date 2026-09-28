package dng

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func mkJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type ent struct {
	tag, typ uint16
	val      uint32
}

// writeIFD writes an IFD of single-valued SHORT/LONG entries at the current end of buf.
func writeIFD(buf *bytes.Buffer, entries []ent, next uint32) {
	le := binary.LittleEndian
	binary.Write(buf, le, uint16(len(entries)))
	for _, e := range entries {
		binary.Write(buf, le, e.tag)
		binary.Write(buf, le, e.typ)
		binary.Write(buf, le, uint32(1))
		if e.typ == typeShort {
			binary.Write(buf, le, uint16(e.val))
			binary.Write(buf, le, uint16(0))
		} else {
			binary.Write(buf, le, e.val)
		}
	}
	binary.Write(buf, le, next)
}

// Layout: IFD0 = small thumbnail (subfile 1) with SubIFDs -> [raw (subfile 0, JPEG), large preview (subfile 1)].
// twoPreviewDNG writes a DNG with a 64×48 preview in IFD0, a 320×240 preview in
// a SubIFD, and a JPEG-compressed raw (subfile 0) that must be ignored.
func twoPreviewDNG(t *testing.T) string {
	t.Helper()
	thumb := mkJPEG(t, 64, 48)
	large := mkJPEG(t, 320, 240)
	rawJunk := mkJPEG(t, 640, 480) // JPEG-compressed but subfile 0: must be ignored

	const ifdSize = 2 + 8*12 + 4
	ifd0 := uint32(8)
	sub1 := ifd0 + ifdSize
	sub2 := sub1 + ifdSize
	subArr := sub2 + ifdSize // two LONG offsets for SubIFDs
	dataStart := subArr + 8
	thumbOff := dataStart
	largeOff := thumbOff + uint32(len(thumb))
	rawOff := largeOff + uint32(len(large))

	var buf bytes.Buffer
	buf.WriteString("II")
	binary.Write(&buf, binary.LittleEndian, uint16(42))
	binary.Write(&buf, binary.LittleEndian, ifd0)

	pad := func(es []ent) []ent { // keep every IFD at 8 entries so offsets are predictable
		for len(es) < 8 {
			es = append(es, ent{0xC000 + uint16(len(es)), typeShort, 0})
		}
		return es
	}
	writeIFD(&buf, pad([]ent{
		{tagNewSubfileType, typeLong, 1},
		{tagCompression, typeShort, 7},
		{tagOrientation, typeShort, 6},
		{tagStripOffsets, typeLong, thumbOff},
		{tagStripByteCounts, typeLong, uint32(len(thumb))},
		{tagSubIFDs, typeLong, 0}, // patched below: count 2 at subArr
	}), 0)
	writeIFD(&buf, pad([]ent{
		{tagNewSubfileType, typeLong, 0},
		{tagCompression, typeShort, 7},
		{tagStripOffsets, typeLong, rawOff},
		{tagStripByteCounts, typeLong, uint32(len(rawJunk))},
	}), 0)
	writeIFD(&buf, pad([]ent{
		{tagNewSubfileType, typeLong, 1},
		{tagCompression, typeShort, 7},
		{tagJPEGIFOffset, typeLong, largeOff},
		{tagJPEGIFByteCount, typeLong, uint32(len(large))},
	}), 0)
	binary.Write(&buf, binary.LittleEndian, sub1)
	binary.Write(&buf, binary.LittleEndian, sub2)
	buf.Write(thumb)
	buf.Write(large)
	buf.Write(rawJunk)

	// Patch SubIFDs entry (6th entry of IFD0): count=2, value=offset of array.
	b := buf.Bytes()
	entryOff := int(ifd0) + 2 + 5*12
	binary.LittleEndian.PutUint32(b[entryOff+4:], 2)
	binary.LittleEndian.PutUint32(b[entryOff+8:], subArr)

	path := filepath.Join(t.TempDir(), "test.dng")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractPicksLargestReducedResolutionJPEG(t *testing.T) {
	path := twoPreviewDNG(t)
	p, err := Extract(path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if p.Width != 320 || p.Height != 240 {
		t.Fatalf("got %dx%d, want 320x240", p.Width, p.Height)
	}
	if p.Orientation != 6 {
		t.Fatalf("orientation %d, want 6", p.Orientation)
	}
}

func TestExtractRejectsNonTIFF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.dng")
	os.WriteFile(path, []byte("not a tiff at all"), 0o644)
	if _, err := Extract(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestPreviewAtLeastPicksSmallestLargeEnough(t *testing.T) {
	path := twoPreviewDNG(t)
	for _, c := range []struct{ min, want int }{{50, 64}, {100, 320}, {5000, 320}} {
		p, err := PreviewAtLeast(path, c.min)
		if err != nil || p.Width != c.want || p.Orientation != 6 {
			t.Errorf("min %d: got %+v %v, want width %d", c.min, p, err, c.want)
		}
	}
}
