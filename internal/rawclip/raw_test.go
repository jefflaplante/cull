package rawclip

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// rawDNG wraps an LJPEG strip in a one-IFD DNG like the M11-P's: subfile 0,
// compression 7, WhiteLevel/BlackLevel, a 2×2 RGGB CFA.
func rawDNG(t *testing.T, strip []byte, width, height int) string {
	t.Helper()
	type ent struct {
		tag, typ uint16
		val      uint32
		bytes    []byte
	}
	const n = 11
	dataOff := uint32(8 + 2 + n*12 + 4)
	ents := []ent{
		{0x00FE, 4, 0, nil}, {0x0100, 4, uint32(width), nil}, {0x0101, 4, uint32(height), nil},
		{0x0102, 3, 16, nil}, {0x0103, 3, 7, nil}, {0x0111, 4, dataOff, nil}, {0x0117, 4, uint32(len(strip)), nil},
		{0x828D, 3, 0, []byte{2, 0, 2, 0}}, {0x828E, 1, 0, []byte{0, 1, 1, 2}},
		{0xC61A, 4, 1023, nil}, {0xC61D, 4, 16383, nil},
	}
	le := binary.LittleEndian
	var b bytes.Buffer
	b.WriteString("II")
	binary.Write(&b, le, uint16(42))
	binary.Write(&b, le, uint32(8))
	binary.Write(&b, le, uint16(n))
	for _, e := range ents {
		binary.Write(&b, le, e.tag)
		binary.Write(&b, le, e.typ)
		count := uint32(1)
		if e.bytes != nil {
			count = 2
			if e.typ == 1 {
				count = 4
			}
		}
		binary.Write(&b, le, count)
		if e.bytes != nil {
			b.Write(e.bytes)
		} else {
			binary.Write(&b, le, e.val)
		}
	}
	binary.Write(&b, le, uint32(0))
	b.Write(strip)
	p := filepath.Join(t.TempDir(), "raw.dng")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMeasureCountsSaturatedSamplesPerChannel(t *testing.T) {
	const w, h = 16, 8 // two components: the raw is 32 columns wide
	samples := [][]uint16{make([]uint16, w*h), make([]uint16, w*h)}
	for c := range samples {
		for i := range samples[c] {
			samples[c][i] = 4000
		}
	}
	for x := 0; x < w; x++ {
		samples[0][x] = 16383 // row 0, even raw columns: red on an RGGB sensor
	}
	p := rawDNG(t, encodeLJPEG(samples, w, h, 2, 14, 1), 2*w, h)
	r, err := Measure(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.WhiteLevel != 16383 || math.Abs(r.HighlightPct-6.25) > 1e-9 {
		t.Fatalf("result %+v", r)
	}
	if r.ChannelPct["R"] != 25 || r.ChannelPct["G"] != 0 || r.ChannelPct["B"] != 0 {
		t.Fatalf("channels %v", r.ChannelPct)
	}
}

func TestMeasureRejectsNonLJPEGRaw(t *testing.T) {
	p := rawDNG(t, []byte{0, 1, 2, 3}, 4, 1)
	if _, err := Measure(p); err == nil {
		t.Fatal("garbage raw accepted")
	}
}
