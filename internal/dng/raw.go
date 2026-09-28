package dng

import (
	"errors"
	"os"
)

const (
	tagWidth         = 0x0100
	tagHeight        = 0x0101
	tagBitsPerSample = 0x0102
	tagTileOffsets   = 0x0144
	tagCFARepeatDim  = 0x828D
	tagCFAPattern    = 0x828E
	tagBlackLevel    = 0xC61A
	tagWhiteLevel    = 0xC61D
	typeByte         = 1
	subfileFullRes   = 0
	compressionLJPEG = 7
)

// Raw locates the full-resolution raw image data in a DNG.
type Raw struct {
	Width, Height, Bits, Compression int
	Offsets, Counts                  []int64 // strips, top to bottom
	Tiled                            bool    // tiled raw (not supported by rawclip)
	WhiteLevel, BlackLevel           int
	CFA                              []byte // 2×2 pattern (0 R, 1 G, 2 B) when present
}

// ReadRaw finds the first full-resolution (NewSubfileType 0) IFD in IFD0's chain
// or its SubIFDs.
func ReadRaw(path string) (*Raw, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	t, ifd0, err := newTIFFReader(f, st.Size())
	if err != nil {
		return nil, err
	}
	visited := map[uint32]bool{}
	queue := []uint32{ifd0}
	for len(queue) > 0 && len(visited) < maxIFDs {
		off := queue[0]
		queue = queue[1:]
		if off == 0 || visited[off] || int64(off) >= st.Size() {
			continue
		}
		visited[off] = true
		entries, next, err := t.readIFD(off)
		if err != nil {
			continue
		}
		if next != 0 {
			queue = append(queue, next)
		}
		if e, ok := entries[tagSubIFDs]; ok {
			if subs, err := t.uints(e); err == nil {
				queue = append(queue, subs...)
			}
		}
		if sub, ok := t.first(entries, tagNewSubfileType); ok && sub != subfileFullRes {
			continue
		}
		if _, ok := entries[tagWidth]; !ok {
			continue
		}
		return t.raw(entries), nil
	}
	return nil, errors.New("no full-resolution raw IFD")
}

func (t *tiffReader) raw(entries map[uint16]ifdEntry) *Raw {
	r := &Raw{}
	get := func(tag uint16) int { v, _ := t.first(entries, tag); return int(v) }
	r.Width, r.Height, r.Bits, r.Compression = get(tagWidth), get(tagHeight), get(tagBitsPerSample), get(tagCompression)
	if v, ok := t.first(entries, tagWhiteLevel); ok {
		r.WhiteLevel = int(v)
	} else if r.Bits > 0 && r.Bits <= 16 {
		r.WhiteLevel = 1<<r.Bits - 1 // the DNG default
	}
	if v, ok := t.first(entries, tagBlackLevel); ok {
		r.BlackLevel = int(v)
	} else if v, ok := t.rational(entries, tagBlackLevel, 0); ok {
		r.BlackLevel = int(v)
	}
	_, r.Tiled = entries[tagTileOffsets]
	offs, _ := t.entryUints(entries, tagStripOffsets)
	cnts, _ := t.entryUints(entries, tagStripByteCounts)
	for i := 0; i < len(offs) && i < len(cnts); i++ {
		r.Offsets = append(r.Offsets, int64(offs[i]))
		r.Counts = append(r.Counts, int64(cnts[i]))
	}
	if dim, err := t.entryUints(entries, tagCFARepeatDim); err == nil && len(dim) == 2 && dim[0] == 2 && dim[1] == 2 {
		if e, ok := entries[tagCFAPattern]; ok && e.typ == typeByte && e.count == 4 {
			r.CFA = append([]byte(nil), e.value[:4]...)
		}
	}
	return r
}
