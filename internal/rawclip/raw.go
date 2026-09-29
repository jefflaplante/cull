// Package rawclip measures highlight clipping in a DNG's raw data, which has
// headroom the tone-mapped preview hides. It decodes the lossless-JPEG raw in
// pure Go, row by row, counting samples at the sensor's white level.
package rawclip

import (
	"fmt"
	"os"

	"github.com/jefflaplante/cull/internal/dng"
)

// Result is the share of raw samples at (or within 0.2% of) the white level.
type Result struct {
	HighlightPct float64            `json:"highlight_pct"`
	ChannelPct   map[string]float64 `json:"channel_pct,omitempty"` // R, G, B when the CFA is 2×2
	WhiteLevel   int                `json:"white_level"`
}

var colorName = [...]string{"R", "G", "B"}

// Measure decodes the raw of a DNG and counts clipped samples. A corrupt raw is
// an error, never a crash: raw clipping is advisory.
func Measure(path string) (res *Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("raw decode failed: %v", r)
		}
	}()
	raw, err := dng.ReadRaw(path)
	if err != nil {
		return nil, err
	}
	if raw.Bits == 0 || raw.WhiteLevel <= raw.BlackLevel {
		return nil, fmt.Errorf("raw bit depth / white level unknown (bits %d, white %d, black %d)", raw.Bits, raw.WhiteLevel, raw.BlackLevel)
	}
	if raw.Compression != 7 || raw.Tiled || len(raw.Offsets) == 0 {
		return nil, fmt.Errorf("raw is not a striped lossless JPEG (compression %d, tiled %v)", raw.Compression, raw.Tiled)
	}
	clipAt := raw.WhiteLevel - max(1, (raw.WhiteLevel-raw.BlackLevel)/500)
	var total, clipped int
	var chTotal, chClipped [3]int
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	y0 := 0
	for s := range raw.Offsets {
		data := make([]byte, raw.Counts[s])
		if _, err := f.ReadAt(data, raw.Offsets[s]); err != nil {
			return nil, fmt.Errorf("read raw strip: %w", err)
		}
		_, h, _, err := decodeLJPEG(data, func(y int, row []uint16) {
			ry := y0 + y
			for x, v := range row {
				hit := int(v) >= clipAt
				total++
				if hit {
					clipped++
				}
				if len(raw.CFA) == 4 {
					c := raw.CFA[(ry%2)*2+x%2]
					if c < 3 {
						chTotal[c]++
						if hit {
							chClipped[c]++
						}
					}
				}
			}
		})
		if err != nil {
			return nil, err
		}
		y0 += h
	}
	if total == 0 {
		return nil, fmt.Errorf("raw has no samples")
	}
	r := &Result{HighlightPct: 100 * float64(clipped) / float64(total), WhiteLevel: raw.WhiteLevel}
	if len(raw.CFA) == 4 {
		r.ChannelPct = map[string]float64{}
		for c := range chTotal {
			if chTotal[c] > 0 {
				r.ChannelPct[colorName[c]] = 100 * float64(chClipped[c]) / float64(chTotal[c])
			}
		}
	}
	return r, nil
}
