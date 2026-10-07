package dng

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tag is one TIFF entry for the test builder: typ 1 (BYTE), 2 (ASCII), 3 (SHORT),
// 4 (LONG), 5 (RATIONAL, values as num/den pairs).
type tag struct {
	id   uint16
	typ  uint16
	ints []uint32
	str  string
}

// buildTIFF lays out IFD0 and, when exif is non-nil, an Exif IFD pointed to by
// 0x8769. Values over 4 bytes go in a data area after each IFD.
func buildTIFF(ifd0, exif []tag) []byte {
	le := binary.LittleEndian
	var out bytes.Buffer
	out.WriteString("II")
	binary.Write(&out, le, uint16(42))
	binary.Write(&out, le, uint32(8))
	write := func(start uint32, tags []tag) []byte {
		var ifd, data bytes.Buffer
		dataStart := start + 2 + uint32(len(tags))*12 + 4
		binary.Write(&ifd, le, uint16(len(tags)))
		for _, t := range tags {
			var raw []byte
			count := uint32(0)
			switch t.typ {
			case 1:
				for _, v := range t.ints {
					raw = append(raw, byte(v))
				}
				count = uint32(len(t.ints))
			case 2:
				raw, count = append([]byte(t.str), 0), uint32(len(t.str)+1)
			case 3:
				for _, v := range t.ints {
					raw = binary.LittleEndian.AppendUint16(raw, uint16(v))
				}
				count = uint32(len(t.ints))
			case 4:
				for _, v := range t.ints {
					raw = binary.LittleEndian.AppendUint32(raw, v)
				}
				count = uint32(len(t.ints))
			case 5:
				for _, v := range t.ints {
					raw = binary.LittleEndian.AppendUint32(raw, v)
				}
				count = uint32(len(t.ints) / 2)
			}
			binary.Write(&ifd, le, t.id)
			binary.Write(&ifd, le, t.typ)
			binary.Write(&ifd, le, count)
			if len(raw) <= 4 {
				v := make([]byte, 4)
				copy(v, raw)
				ifd.Write(v)
			} else {
				binary.Write(&ifd, le, dataStart+uint32(data.Len()))
				data.Write(raw)
				if data.Len()%2 == 1 {
					data.WriteByte(0)
				}
			}
		}
		binary.Write(&ifd, le, uint32(0))
		return append(ifd.Bytes(), data.Bytes()...)
	}
	if exif != nil {
		// Exif IFD goes after IFD0; IFD0 size is known once its data is laid out,
		// so lay IFD0 out with a placeholder pointer first, then patch it.
		ifd0 = append(ifd0, tag{id: 0x8769, typ: 4, ints: []uint32{0}})
		first := write(8, ifd0)
		ifd0[len(ifd0)-1].ints[0] = 8 + uint32(len(first))
		out.Write(write(8, ifd0))
		out.Write(write(8+uint32(len(first)), exif))
	} else {
		out.Write(write(8, ifd0))
	}
	return out.Bytes()
}

func TestReadExifLeicaStyle(t *testing.T) {
	// As on a real M11-P: no FNumber, an APEX ApertureValue estimate, whole seconds.
	b := buildTIFF(
		[]tag{{id: 0x010F, typ: 2, str: "Leica Camera AG"}, {id: 0x0110, typ: 2, str: "LEICA M11-P"}},
		[]tag{
			{id: 0x829A, typ: 5, ints: []uint32{1, 125}},
			{id: 0x8827, typ: 3, ints: []uint32{400}},
			{id: 0x9003, typ: 2, str: "2025:12:28 00:06:00"},
			{id: 0x9202, typ: 5, ints: []uint32{450, 100}},
			{id: 0x920A, typ: 5, ints: []uint32{35, 1}},
			{id: 0xA432, typ: 5, ints: []uint32{35, 1, 35, 1, 200, 100, 200, 100}},
			{id: 0xA434, typ: 2, str: "Summicron-M 1:2/35 ASPH."},
		})
	p := filepath.Join(t.TempDir(), "x.dng")
	os.WriteFile(p, b, 0o644)
	e, err := ReadExif(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Make != "Leica Camera AG" || e.Model != "LEICA M11-P" || e.Lens != "Summicron-M 1:2/35 ASPH." || e.ISO != 400 {
		t.Fatalf("strings/ISO: %+v", e)
	}
	if e.ExposureTime != 1.0/125 || e.FocalLength != 35 || e.MaxAperture != 2 {
		t.Fatalf("rationals: %+v", e)
	}
	if !e.FNumberEstimated || e.FNumber < 4.7 || e.FNumber > 4.8 { // 2^(4.5/2) = 4.76
		t.Fatalf("aperture from APEX: %+v", e)
	}
	ct, ok := e.CaptureTime()
	if !ok || !ct.Equal(time.Date(2025, 12, 28, 0, 6, 0, 0, time.UTC)) {
		t.Fatalf("capture time %v %v", ct, ok)
	}
	want := "1/125 s, ~f/4.8 (camera estimate), ISO 400, 35 mm, Summicron-M 1:2/35 ASPH."
	if got := e.Summary(); got != want {
		t.Fatalf("summary\n got %q\nwant %q", got, want)
	}
}

func TestReadExifFNumberSubSecAndMissing(t *testing.T) {
	b := buildTIFF(nil, []tag{
		{id: 0x829A, typ: 5, ints: []uint32{1, 2}},
		{id: 0x829D, typ: 5, ints: []uint32{14, 10}},
		{id: 0x9003, typ: 2, str: "2026:01:02 03:04:05"},
		{id: 0x9291, typ: 2, str: "25"},
	})
	p := filepath.Join(t.TempDir(), "x.dng")
	os.WriteFile(p, b, 0o644)
	e, err := ReadExif(p)
	if err != nil || e.FNumber != 1.4 || e.FNumberEstimated {
		t.Fatalf("fnumber: %+v %v", e, err)
	}
	ct, _ := e.CaptureTime()
	if ct.Nanosecond() != 250_000_000 {
		t.Fatalf("subsec: %v", ct)
	}
	if got := e.Summary(); got != "1/2 s, f/1.4" {
		t.Fatalf("summary %q", got)
	}

	noExif := filepath.Join(t.TempDir(), "y.dng")
	os.WriteFile(noExif, buildTIFF([]tag{{id: 0x0110, typ: 2, str: "X"}}, nil), 0o644)
	e, err = ReadExif(noExif)
	if err != nil || e.Model != "X" || e.Summary() != "" {
		t.Fatalf("no exif IFD: %+v %v", e, err)
	}
	if _, ok := e.CaptureTime(); ok {
		t.Fatal("capture time without DateTimeOriginal")
	}
}

// CameraTime round-trips to exactly CaptureTime's time (sub-seconds included), so
// grouping by a recorded camera_time is grouping by the camera's EXIF.
func TestCameraTimeRoundTrip(t *testing.T) {
	for _, e := range []Exif{
		{DateTimeOriginal: "2025:12:28 00:05:59"},
		{DateTimeOriginal: "2025:12:28 00:05:59", SubSec: "42"},
		{DateTimeOriginal: "2025:12:28 00:05:59", SubSec: "123456789"},
		{DateTimeOriginal: "2025:12:28 00:05:59", SubSec: "007"},
	} {
		want, _ := e.CaptureTime()
		s := e.CameraTime()
		got, ok := ParseCameraTime(s)
		if !ok || !got.Equal(want) || got.Location() != want.Location() {
			t.Errorf("%+v: %q → %v %v, want %v", e, s, got, ok, want)
		}
	}
	if s := (Exif{DateTimeOriginal: "    :  :     :  :  "}).CameraTime(); s != "" {
		t.Errorf("unparseable: %q", s)
	}
	if _, ok := ParseCameraTime(""); ok {
		t.Error(`"" parsed`)
	}
	if s := (Exif{DateTimeOriginal: "2025:12:28 00:05:59", SubSec: "42"}).CameraTime(); s != "2025-12-28T00:05:59.42" {
		t.Errorf("format: %q", s)
	}
}
