package dng

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tagMake             = 0x010F
	tagModel            = 0x0110
	tagExifIFD          = 0x8769
	tagExposureTime     = 0x829A
	tagFNumber          = 0x829D
	tagISO              = 0x8827
	tagDateTimeOriginal = 0x9003
	tagSubSecOriginal   = 0x9291
	tagApertureValue    = 0x9202
	tagFocalLength      = 0x920A
	tagLensSpec         = 0xA432
	tagLensModel        = 0xA434

	typeASCII     = 2
	typeRational  = 5
	typeSRational = 10
)

// Exif is the shooting context passed to the model and used for burst grouping.
// Leica M bodies have no aperture coupling: there is no FNumber tag, and the
// APEX ApertureValue is the camera's estimate from its light meter.
type Exif struct {
	Make             string  `json:"make,omitempty"`
	Model            string  `json:"model,omitempty"`
	Lens             string  `json:"lens,omitempty"`
	ExposureTime     float64 `json:"exposure_time,omitempty"` // seconds
	FNumber          float64 `json:"f_number,omitempty"`
	FNumberEstimated bool    `json:"f_number_estimated,omitempty"`
	ISO              int     `json:"iso,omitempty"`
	FocalLength      float64 `json:"focal_length,omitempty"` // mm; absent for uncoded lenses
	MaxAperture      float64 `json:"max_aperture,omitempty"` // lens specification, widest f-number
	DateTimeOriginal string  `json:"date_time_original,omitempty"`
	SubSec           string  `json:"subsec,omitempty"`
}

// ReadExif reads IFD0 and the Exif IFD. Missing tags are left zero.
func ReadExif(path string) (Exif, error) {
	var e Exif
	f, err := os.Open(path)
	if err != nil {
		return e, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return e, err
	}
	t, ifd0, err := newTIFFReader(f, st.Size())
	if err != nil {
		return e, err
	}
	entries, _, err := t.readIFD(ifd0)
	if err != nil {
		return e, err
	}
	e.Make, _ = t.ascii(entries, tagMake)
	e.Model, _ = t.ascii(entries, tagModel)
	e.DateTimeOriginal, _ = t.ascii(entries, tagDateTimeOriginal)
	if off, ok := t.first(entries, tagExifIFD); ok && off != 0 {
		if ex, _, err := t.readIFD(off); err == nil {
			t.fillExif(&e, ex)
		}
	}
	return e, nil
}

func (t *tiffReader) fillExif(e *Exif, ex map[uint16]ifdEntry) {
	if v, ok := t.rational(ex, tagExposureTime, 0); ok {
		e.ExposureTime = v
	}
	if v, ok := t.rational(ex, tagFNumber, 0); ok && v > 0 {
		e.FNumber = v
	} else if av, ok := t.rational(ex, tagApertureValue, 0); ok {
		e.FNumber = math.Round(math.Pow(2, av/2)*10) / 10
		e.FNumberEstimated = true
	}
	if v, ok := t.first(ex, tagISO); ok {
		e.ISO = int(v)
	}
	if v, ok := t.rational(ex, tagFocalLength, 0); ok {
		e.FocalLength = v
	}
	if v, ok := t.rational(ex, tagLensSpec, 2); ok {
		e.MaxAperture = v
	}
	if s, ok := t.ascii(ex, tagLensModel); ok {
		e.Lens = s
	}
	if s, ok := t.ascii(ex, tagDateTimeOriginal); ok {
		e.DateTimeOriginal = s
	}
	if s, ok := t.ascii(ex, tagSubSecOriginal); ok {
		e.SubSec = s
	}
}

// ascii reads an ASCII tag, trimmed of NULs and spaces.
func (t *tiffReader) ascii(entries map[uint16]ifdEntry, tag uint16) (string, bool) {
	e, ok := entries[tag]
	if !ok || e.typ != typeASCII || e.count == 0 || e.count > 4096 {
		return "", false
	}
	b := e.value[:]
	if e.count > 4 {
		b = make([]byte, e.count)
		if _, err := t.r.ReadAt(b, int64(t.bo.Uint32(e.value[:]))); err != nil {
			return "", false
		}
	}
	return strings.TrimSpace(strings.TrimRight(string(b[:min(int(e.count), len(b))]), "\x00")), true
}

// rational reads the i-th (signed) RATIONAL of a tag.
func (t *tiffReader) rational(entries map[uint16]ifdEntry, tag uint16, i int) (float64, bool) {
	e, ok := entries[tag]
	if !ok || (e.typ != typeRational && e.typ != typeSRational) || i >= int(e.count) {
		return 0, false
	}
	var b [8]byte
	if _, err := t.r.ReadAt(b[:], int64(t.bo.Uint32(e.value[:]))+int64(8*i)); err != nil {
		return 0, false
	}
	num, den := t.bo.Uint32(b[:4]), t.bo.Uint32(b[4:])
	if den == 0 {
		return 0, false
	}
	if e.typ == typeSRational {
		return float64(int32(num)) / float64(int32(den)), true
	}
	return float64(num) / float64(den), true
}

// CaptureTime parses DateTimeOriginal (+SubSec). The camera's local time is
// treated as UTC: it only orders frames of one shoot.
func (e Exif) CaptureTime() (time.Time, bool) {
	t, err := time.Parse("2006:01:02 15:04:05", e.DateTimeOriginal)
	if err != nil {
		return time.Time{}, false
	}
	if ss := strings.TrimSpace(e.SubSec); ss != "" {
		if f, err := strconv.ParseFloat("0."+ss, 64); err == nil {
			t = t.Add(time.Duration(f * float64(time.Second)))
		}
	}
	return t, true
}

// Summary is the one-line shooting context for the model, e.g.
// "1/125 s, ~f/4.8 (camera estimate), ISO 400, 35 mm, Summicron-M 1:2/35 ASPH.".
func (e Exif) Summary() string {
	var parts []string
	switch {
	case e.ExposureTime <= 0:
	case e.ExposureTime < 1:
		parts = append(parts, fmt.Sprintf("1/%d s", int(math.Round(1/e.ExposureTime))))
	default:
		parts = append(parts, strconv.FormatFloat(e.ExposureTime, 'g', 3, 64)+" s")
	}
	if e.FNumber > 0 {
		if e.FNumberEstimated {
			parts = append(parts, fmt.Sprintf("~f/%.1f (camera estimate)", e.FNumber))
		} else {
			parts = append(parts, "f/"+strconv.FormatFloat(e.FNumber, 'f', -1, 64))
		}
	}
	if e.ISO > 0 {
		parts = append(parts, fmt.Sprintf("ISO %d", e.ISO))
	}
	if e.FocalLength > 0 {
		parts = append(parts, strconv.FormatFloat(e.FocalLength, 'f', -1, 64)+" mm")
	}
	if e.Lens != "" {
		parts = append(parts, e.Lens)
	}
	return strings.Join(parts, ", ")
}
