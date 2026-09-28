package dng

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRawFindsFullResolutionIFD(t *testing.T) {
	// M11-P layout: the raw is IFD0 (subfile 0, lossless JPEG, one strip).
	b := buildTIFF([]tag{
		{id: 0x00FE, typ: 4, ints: []uint32{0}},
		{id: 0x0100, typ: 4, ints: []uint32{9536}},
		{id: 0x0101, typ: 4, ints: []uint32{6336}},
		{id: 0x0102, typ: 3, ints: []uint32{16}},
		{id: 0x0103, typ: 3, ints: []uint32{7}},
		{id: 0x0111, typ: 4, ints: []uint32{4892606}},
		{id: 0x0117, typ: 4, ints: []uint32{66295808}},
		{id: 0x828D, typ: 3, ints: []uint32{2, 2}},
		{id: 0x828E, typ: 1, ints: []uint32{0, 1, 1, 2}},
		{id: 0xC61A, typ: 4, ints: []uint32{1023}},
		{id: 0xC61D, typ: 4, ints: []uint32{16383}},
	}, nil)
	p := filepath.Join(t.TempDir(), "r.dng")
	os.WriteFile(p, b, 0o644)
	r, err := ReadRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Width != 9536 || r.Height != 6336 || r.Compression != 7 || r.Tiled || len(r.Offsets) != 1 ||
		r.Offsets[0] != 4892606 || r.Counts[0] != 66295808 || r.WhiteLevel != 16383 || r.BlackLevel != 1023 ||
		string(r.CFA) != "\x00\x01\x01\x02" {
		t.Fatalf("raw %+v", r)
	}

	noRaw := filepath.Join(t.TempDir(), "n.dng")
	os.WriteFile(noRaw, buildTIFF([]tag{{id: 0x00FE, typ: 4, ints: []uint32{1}}}, nil), 0o644)
	if _, err := ReadRaw(noRaw); err == nil {
		t.Fatal("a file with only a preview has no raw")
	}
}

func TestReadRawDefaultsWhiteLevelToBitDepth(t *testing.T) {
	b := buildTIFF([]tag{
		{id: 0x00FE, typ: 4, ints: []uint32{0}}, {id: 0x0100, typ: 4, ints: []uint32{8}},
		{id: 0x0101, typ: 4, ints: []uint32{8}}, {id: 0x0102, typ: 3, ints: []uint32{14}},
		{id: 0x0103, typ: 3, ints: []uint32{7}},
	}, nil)
	p := filepath.Join(t.TempDir(), "r.dng")
	os.WriteFile(p, b, 0o644)
	if r, err := ReadRaw(p); err != nil || r.WhiteLevel != 16383 {
		t.Fatalf("14-bit raw without WhiteLevel: %+v %v", r, err)
	}
}
