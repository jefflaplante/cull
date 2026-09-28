package rawclip

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var errNotLJPEG = errors.New("not a lossless JPEG stream")

// decodeLJPEG decodes a lossless (ITU T.81 process 14, SOF3) JPEG as used for DNG
// raw data and calls row for each decoded line: w×comps interleaved samples.
// row must not keep the slice. Only what DNG writers use is supported: one scan,
// all components interleaved with 1×1 sampling, no restart intervals.
func decodeLJPEG(data []byte, row func(y int, samples []uint16)) (w, h, comps int, err error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0, 0, errNotLJPEG
	}
	var tables [4]*huff
	var precision int
	var compIDs []byte
	pos := 2
	for {
		if pos+4 > len(data) || data[pos] != 0xFF {
			return 0, 0, 0, fmt.Errorf("%w: bad marker at %d", errNotLJPEG, pos)
		}
		marker := data[pos+1]
		if marker == 0xFF { // fill byte
			pos++
			continue
		}
		n := int(binary.BigEndian.Uint16(data[pos+2:]))
		if n < 2 || pos+2+n > len(data) {
			return 0, 0, 0, fmt.Errorf("%w: truncated segment %02X", errNotLJPEG, marker)
		}
		body := data[pos+4 : pos+2+n]
		switch marker {
		case 0xC4: // DHT: one or more tables
			for len(body) >= 17 {
				id := body[0] & 0x0F
				var bits [16]byte
				copy(bits[:], body[1:17])
				total := 0
				for _, b := range bits {
					total += int(b)
				}
				if id > 3 || len(body) < 17+total {
					return 0, 0, 0, fmt.Errorf("%w: bad Huffman table", errNotLJPEG)
				}
				h, err := buildHuff(bits, body[17:17+total])
				if err != nil {
					return 0, 0, 0, fmt.Errorf("%w: %v", errNotLJPEG, err)
				}
				tables[id] = h
				body = body[17+total:]
			}
		case 0xC3: // SOF3
			if len(body) < 6 {
				return 0, 0, 0, fmt.Errorf("%w: short SOF3", errNotLJPEG)
			}
			precision = int(body[0])
			h, w, comps = int(binary.BigEndian.Uint16(body[1:])), int(binary.BigEndian.Uint16(body[3:])), int(body[5])
			if comps < 1 || len(body) < 6+3*comps {
				return 0, 0, 0, fmt.Errorf("%w: bad SOF3 components", errNotLJPEG)
			}
			for c := 0; c < comps; c++ {
				if body[6+3*c+1] != 0x11 {
					return 0, 0, 0, fmt.Errorf("%w: subsampled components unsupported", errNotLJPEG)
				}
				compIDs = append(compIDs, body[6+3*c])
			}
		case 0xC0, 0xC1, 0xC2, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
			return 0, 0, 0, fmt.Errorf("%w: SOF%X is not lossless Huffman", errNotLJPEG, marker&0x0F)
		case 0xDD: // DRI
			if len(body) >= 2 && binary.BigEndian.Uint16(body) != 0 {
				return 0, 0, 0, fmt.Errorf("%w: restart intervals unsupported", errNotLJPEG)
			}
		case 0xDA: // SOS: entropy-coded data follows
			if precision == 0 || w == 0 || h == 0 {
				return 0, 0, 0, fmt.Errorf("%w: SOS before SOF3", errNotLJPEG)
			}
			ns := int(body[0])
			if ns != comps || len(body) < 1+2*ns+3 {
				return 0, 0, 0, fmt.Errorf("%w: scan must interleave all components", errNotLJPEG)
			}
			hs := make([]*huff, comps)
			for c := 0; c < comps; c++ {
				td := body[1+2*c+1] >> 4
				if td > 3 || tables[td] == nil {
					return 0, 0, 0, fmt.Errorf("%w: missing Huffman table %d", errNotLJPEG, td)
				}
				hs[c] = tables[td]
			}
			sel := int(body[1+2*ns])
			pt := int(body[1+2*ns+2] & 0x0F)
			if sel < 1 || sel > 7 || pt >= precision {
				return 0, 0, 0, fmt.Errorf("%w: predictor %d, point transform %d", errNotLJPEG, sel, pt)
			}
			err = scan(&bitReader{data: data[pos+2+n:]}, hs, w, h, comps, precision, sel, pt, row)
			return w, h, comps, err
		}
		pos += 2 + n
	}
}

func scan(br *bitReader, hs []*huff, w, h, comps, precision, sel, pt int, row func(int, []uint16)) error {
	stride := w * comps
	prev, cur := make([]int32, stride), make([]int32, stride)
	out := make([]uint16, stride)
	first := int32(1) << (precision - pt - 1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < comps; c++ {
				i := x*comps + c
				var pred int32
				switch {
				case x == 0 && y == 0:
					pred = first
				case y == 0:
					pred = cur[i-comps]
				case x == 0:
					pred = prev[i]
				default:
					ra, rb, rc := cur[i-comps], prev[i], prev[i-comps]
					switch sel {
					case 1:
						pred = ra
					case 2:
						pred = rb
					case 3:
						pred = rc
					case 4:
						pred = ra + rb - rc
					case 5:
						pred = ra + ((rb - rc) >> 1)
					case 6:
						pred = rb + ((ra - rc) >> 1)
					default:
						pred = (ra + rb) >> 1
					}
				}
				cat, err := hs[c].decode(br)
				if err != nil {
					return fmt.Errorf("row %d: %w", y, err)
				}
				if cat > 16 {
					return fmt.Errorf("row %d: invalid difference category %d", y, cat)
				}
				var diff int32
				switch {
				case cat == 16:
					diff = 32768
				case cat > 0:
					v := int32(br.bits(uint(cat)))
					if v < 1<<(cat-1) {
						v -= 1<<cat - 1
					}
					diff = v
				}
				cur[i] = (pred + diff) & 0xFFFF
				out[i] = uint16(cur[i] << pt)
			}
		}
		if br.overrun > 8 {
			return fmt.Errorf("row %d: entropy data ended early", y)
		}
		row(y, out)
		prev, cur = cur, prev
	}
	return nil
}

// huff is a canonical Huffman decoder with a 9-bit lookup table.
type huff struct {
	lut     [1 << 9]uint16 // len<<8 | symbol; 0 = longer code
	maxcode [18]int32
	valptr  [17]int32
	mincode [17]int32
	vals    []byte
}

func buildHuff(bits [16]byte, vals []byte) (*huff, error) {
	h := &huff{vals: vals}
	code, k := int32(0), int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(bits[l-1])
		h.valptr[l], h.mincode[l] = k, code
		if code+n > 1<<l { // more codes of this length than exist: a corrupt table
			return nil, fmt.Errorf("Huffman table over-subscribed at length %d", l)
		}
		for i := int32(0); i < n; i++ {
			if l <= 9 {
				c := code << (9 - l)
				for j := int32(0); j < 1<<(9-l); j++ {
					h.lut[c|j] = uint16(l)<<8 | uint16(vals[k])
				}
			}
			code++
			k++
		}
		h.maxcode[l] = code - 1
		if n == 0 {
			h.maxcode[l] = -1
		}
		code <<= 1
	}
	h.maxcode[17] = 1<<31 - 1
	return h, nil
}

func (h *huff) decode(br *bitReader) (int, error) {
	if e := h.lut[br.peek(9)]; e != 0 {
		br.skip(uint(e >> 8))
		return int(e & 0xFF), nil
	}
	code := int32(br.bits(9))
	for l := 10; l <= 16; l++ {
		code = code<<1 | int32(br.bits(1))
		if code <= h.maxcode[l] {
			return int(h.vals[h.valptr[l]+code-h.mincode[l]]), nil
		}
	}
	return 0, errors.New("bad Huffman code")
}

// bitReader reads entropy-coded bits, undoing 0xFF00 stuffing and feeding zeros
// once a marker (end of scan) is reached.
type bitReader struct {
	data    []byte
	pos     int
	acc     uint64
	n       uint
	marker  bool
	overrun int // zero bytes fed past the end
}

func (br *bitReader) fill() {
	for br.n <= 56 {
		var b byte
		if !br.marker && br.pos < len(br.data) {
			b = br.data[br.pos]
			br.pos++
			if b == 0xFF {
				if br.pos < len(br.data) && br.data[br.pos] == 0x00 {
					br.pos++
				} else {
					br.marker, b = true, 0
				}
			}
		} else {
			br.overrun++
		}
		br.acc = br.acc<<8 | uint64(b)
		br.n += 8
	}
}

func (br *bitReader) peek(n uint) uint32 {
	if br.n < n {
		br.fill()
	}
	return uint32(br.acc>>(br.n-n)) & (1<<n - 1)
}

func (br *bitReader) skip(n uint) { br.n -= n }

func (br *bitReader) bits(n uint) uint32 {
	v := br.peek(n)
	br.skip(n)
	return v
}
