// Package dng extracts embedded JPEG previews from DNG files without decoding raw data.
//
// DNG is TIFF-based. Previews live in IFDs with NewSubfileType bit 0 set (reduced
// resolution) and JPEG compression, reachable from IFD0's next-IFD chain or its
// SubIFDs. The main raw image (NewSubfileType 0) may also be JPEG-compressed
// (lossless JPEG), so the subfile bit is what distinguishes a preview.
package dng

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	tagNewSubfileType  = 0x00FE
	tagImageWidth      = 0x0100
	tagImageLength     = 0x0101
	tagCompression     = 0x0103
	tagStripOffsets    = 0x0111
	tagOrientation     = 0x0112
	tagStripByteCounts = 0x0117
	tagSubIFDs         = 0x014A
	tagJPEGIFOffset    = 0x0201
	tagJPEGIFByteCount = 0x0202

	typeShort = 3
	typeLong  = 4
	typeIFD   = 13

	maxIFDs = 64
)

// Preview is an embedded JPEG plus the orientation needed to display it.
type Preview struct {
	Data        []byte
	Width       int
	Height      int
	Orientation int    // EXIF orientation from IFD0 (1 = normal)
	Source      string // "tiff-ifd" or "exiftool:<tag>"
}

// LongEdge returns the preview's longest dimension.
func (p *Preview) LongEdge() int {
	if p.Width > p.Height {
		return p.Width
	}
	return p.Height
}

type ifdEntry struct {
	tag, typ uint16
	count    uint32
	value    [4]byte
}

type tiffReader struct {
	r    io.ReaderAt
	bo   binary.ByteOrder
	size int64
}

type candidate struct{ offset, length int64 }

// Best returns the largest embedded preview. If the native parser finds nothing
// with a long edge >= minLongEdge and exiftool is on PATH, exiftool is tried too
// and the larger result wins.
func Best(path string, minLongEdge int) (*Preview, error) {
	native, nerr := Extract(path)
	if native != nil && native.LongEdge() >= minLongEdge {
		return native, nil
	}
	alt, aerr := ExtractWithExiftool(path)
	switch {
	case native == nil && alt == nil:
		return nil, fmt.Errorf("no preview found: native: %v; exiftool: %v", nerr, aerr)
	case native == nil:
		return alt, nil
	case alt == nil || alt.LongEdge() <= native.LongEdge():
		return native, nil
	default:
		return alt, nil
	}
}

// Extract parses TIFF structure directly. It reads only IFDs and the preview bytes,
// never the raw image data, so memory stays bounded for 100MB+ files.
func Extract(path string) (*Preview, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cands, orientation, err := findCandidates(f, st.Size())
	if err != nil {
		return nil, err
	}
	var best *Preview
	for _, c := range cands {
		data := make([]byte, c.length)
		if _, err := f.ReadAt(data, c.offset); err != nil {
			continue
		}
		if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
			continue // not a baseline JPEG stream
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			continue
		}
		if best == nil || cfg.Width*cfg.Height > best.Width*best.Height {
			best = &Preview{Data: data, Width: cfg.Width, Height: cfg.Height, Orientation: orientation, Source: "tiff-ifd"}
		}
	}
	if best == nil {
		return nil, errors.New("no decodable JPEG preview in TIFF structure")
	}
	return best, nil
}

func findCandidates(r io.ReaderAt, size int64) ([]candidate, int, error) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return nil, 0, fmt.Errorf("read header: %w", err)
	}
	var bo binary.ByteOrder
	switch string(hdr[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, 0, errors.New("not a TIFF/DNG file")
	}
	if bo.Uint16(hdr[2:4]) != 42 {
		return nil, 0, errors.New("unsupported TIFF variant (BigTIFF not supported)")
	}
	t := &tiffReader{r: r, bo: bo, size: size}

	orientation := 1
	var cands []candidate
	visited := map[uint32]bool{}
	queue := []uint32{bo.Uint32(hdr[4:8])}
	first := true
	for len(queue) > 0 && len(visited) < maxIFDs {
		off := queue[0]
		queue = queue[1:]
		if off == 0 || visited[off] || int64(off) >= size {
			continue
		}
		visited[off] = true
		entries, next, err := t.readIFD(off)
		if err != nil {
			if first {
				return nil, 0, err
			}
			continue
		}
		if first {
			if o, ok := t.first(entries, tagOrientation); ok && o >= 1 && o <= 8 {
				orientation = int(o)
			}
			first = false
		}
		if next != 0 {
			queue = append(queue, next)
		}
		if e, ok := entries[tagSubIFDs]; ok {
			if subs, err := t.uints(e); err == nil {
				queue = append(queue, subs...)
			}
		}
		if c, ok := t.jpegCandidate(entries); ok {
			cands = append(cands, c)
		}
	}
	return cands, orientation, nil
}

func (t *tiffReader) readIFD(off uint32) (map[uint16]ifdEntry, uint32, error) {
	var nb [2]byte
	if _, err := t.r.ReadAt(nb[:], int64(off)); err != nil {
		return nil, 0, fmt.Errorf("read IFD count at %d: %w", off, err)
	}
	n := int(t.bo.Uint16(nb[:]))
	if n == 0 || n > 4096 {
		return nil, 0, fmt.Errorf("implausible IFD entry count %d at %d", n, off)
	}
	buf := make([]byte, n*12+4)
	if _, err := t.r.ReadAt(buf, int64(off)+2); err != nil {
		return nil, 0, fmt.Errorf("read IFD at %d: %w", off, err)
	}
	entries := make(map[uint16]ifdEntry, n)
	for i := 0; i < n; i++ {
		b := buf[i*12:]
		e := ifdEntry{tag: t.bo.Uint16(b[0:2]), typ: t.bo.Uint16(b[2:4]), count: t.bo.Uint32(b[4:8])}
		copy(e.value[:], b[8:12])
		entries[e.tag] = e
	}
	return entries, t.bo.Uint32(buf[n*12:]), nil
}

// uints reads SHORT/LONG/IFD-typed values, inline or at an offset.
func (t *tiffReader) uints(e ifdEntry) ([]uint32, error) {
	var sz int
	switch e.typ {
	case typeShort:
		sz = 2
	case typeLong, typeIFD:
		sz = 4
	default:
		return nil, fmt.Errorf("tag 0x%04x: unsupported type %d", e.tag, e.typ)
	}
	if e.count == 0 || e.count > 1<<16 {
		return nil, fmt.Errorf("tag 0x%04x: bad count %d", e.tag, e.count)
	}
	total := sz * int(e.count)
	var data []byte
	if total <= 4 {
		data = e.value[:total]
	} else {
		data = make([]byte, total)
		if _, err := t.r.ReadAt(data, int64(t.bo.Uint32(e.value[:]))); err != nil {
			return nil, err
		}
	}
	out := make([]uint32, e.count)
	for i := range out {
		if sz == 2 {
			out[i] = uint32(t.bo.Uint16(data[i*2:]))
		} else {
			out[i] = t.bo.Uint32(data[i*4:])
		}
	}
	return out, nil
}

func (t *tiffReader) first(entries map[uint16]ifdEntry, tag uint16) (uint32, bool) {
	e, ok := entries[tag]
	if !ok {
		return 0, false
	}
	v, err := t.uints(e)
	if err != nil {
		return 0, false
	}
	return v[0], true
}

func (t *tiffReader) jpegCandidate(entries map[uint16]ifdEntry) (candidate, bool) {
	subfile, _ := t.first(entries, tagNewSubfileType)
	if subfile&1 == 0 {
		return candidate{}, false // primary (raw) image
	}
	comp, _ := t.first(entries, tagCompression)
	if comp != 6 && comp != 7 {
		return candidate{}, false
	}
	off, okOff := t.first(entries, tagJPEGIFOffset)
	n, okLen := t.first(entries, tagJPEGIFByteCount)
	if !(okOff && okLen) {
		offs, err1 := t.entryUints(entries, tagStripOffsets)
		cnts, err2 := t.entryUints(entries, tagStripByteCounts)
		if err1 != nil || err2 != nil || len(offs) != 1 || len(cnts) != 1 {
			return candidate{}, false // tiled or multi-strip previews not handled
		}
		off, n = offs[0], cnts[0]
	}
	if n < 4 || int64(off)+int64(n) > t.size {
		return candidate{}, false
	}
	return candidate{offset: int64(off), length: int64(n)}, true
}

func (t *tiffReader) entryUints(entries map[uint16]ifdEntry, tag uint16) ([]uint32, error) {
	e, ok := entries[tag]
	if !ok {
		return nil, fmt.Errorf("missing tag 0x%04x", tag)
	}
	return t.uints(e)
}

// ExtractWithExiftool shells out to exiftool, which knows vendor-specific preview
// locations (e.g. MakerNotes) that a plain IFD walk misses.
func ExtractWithExiftool(path string) (*Preview, error) {
	bin, err := exec.LookPath("exiftool")
	if err != nil {
		return nil, errors.New("exiftool not on PATH")
	}
	var best *Preview
	for _, tag := range []string{"JpgFromRaw", "PreviewImage", "OtherImage"} {
		out, err := exec.Command(bin, "-b", "-"+tag, path).Output()
		if err != nil || len(out) < 2 || out[0] != 0xFF || out[1] != 0xD8 {
			continue
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			continue
		}
		if best == nil || cfg.Width*cfg.Height > best.Width*best.Height {
			best = &Preview{Data: out, Width: cfg.Width, Height: cfg.Height, Source: "exiftool:" + tag}
		}
	}
	if best == nil {
		return nil, errors.New("exiftool found no JPEG preview")
	}
	best.Orientation = 1
	if out, err := exec.Command(bin, "-n", "-s3", "-Orientation", path).Output(); err == nil {
		if o, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && o >= 1 && o <= 8 {
			best.Orientation = o
		}
	}
	return best, nil
}
