package rawclip

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// encodeLJPEG is a test-only lossless-JPEG (SOF3) encoder: one Huffman table with
// a 5-bit code per difference category 0..16, components interleaved per sample.
func encodeLJPEG(samples [][]uint16, w, h, comps, precision, predictor int) []byte {
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8})
	// DHT: class 0, id 0, 17 symbols of length 5.
	bits := make([]byte, 16)
	bits[4] = 17
	dht := []byte{0x00}
	dht = append(dht, bits...)
	for s := 0; s <= 16; s++ {
		dht = append(dht, byte(s))
	}
	seg := func(marker byte, body []byte) {
		out.Write([]byte{0xFF, marker, byte((len(body) + 2) >> 8), byte(len(body) + 2)})
		out.Write(body)
	}
	seg(0xC4, dht)
	sof := []byte{byte(precision), byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(comps)}
	for c := 0; c < comps; c++ {
		sof = append(sof, byte(c+1), 0x11, 0)
	}
	seg(0xC3, sof)
	sos := []byte{byte(comps)}
	for c := 0; c < comps; c++ {
		sos = append(sos, byte(c+1), 0x00)
	}
	sos = append(sos, byte(predictor), 0, 0)
	seg(0xDA, sos)

	var acc uint64
	var n uint
	put := func(v uint32, bitsN uint) {
		acc = acc<<bitsN | uint64(v)&(1<<bitsN-1)
		n += bitsN
		for n >= 8 {
			b := byte(acc >> (n - 8))
			out.WriteByte(b)
			if b == 0xFF {
				out.WriteByte(0x00)
			}
			n -= 8
		}
	}
	at := func(c, x, y int) int { return int(samples[c][y*w+x]) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < comps; c++ {
				pred := predict(predictor, precision, x, y, func(dx, dy int) int { return at(c, x+dx, y+dy) })
				diff := (at(c, x, y) - pred) & 0xFFFF
				if diff >= 0x8000 {
					diff -= 0x10000
				}
				var cat int
				if diff < 0 {
					cat = bitLen(-diff)
				} else {
					cat = bitLen(diff)
				}
				put(uint32(cat), 5)
				if cat > 0 && cat < 16 {
					v := diff
					if diff < 0 {
						v = diff + (1<<cat - 1)
					}
					put(uint32(v), uint(cat))
				}
			}
		}
	}
	if n > 0 {
		put(0x7F, 8-n) // pad with ones
	}
	out.Write([]byte{0xFF, 0xD9})
	return out.Bytes()
}

func bitLen(v int) int {
	n := 0
	for ; v > 0; v >>= 1 {
		n++
	}
	return n
}

// predict mirrors T.81: first sample 2^(P-1); first row uses left; first column above.
func predict(sel, precision, x, y int, px func(dx, dy int) int) int {
	switch {
	case x == 0 && y == 0:
		return 1 << (precision - 1)
	case y == 0:
		return px(-1, 0)
	case x == 0:
		return px(0, -1)
	}
	ra, rb, rc := px(-1, 0), px(0, -1), px(-1, -1)
	switch sel {
	case 1:
		return ra
	case 2:
		return rb
	case 3:
		return rc
	case 4:
		return ra + rb - rc
	case 5:
		return ra + ((rb - rc) >> 1)
	case 6:
		return rb + ((ra - rc) >> 1)
	}
	return (ra + rb) >> 1
}

func TestLJPEGRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, comps := range []int{1, 2} {
		for pred := 1; pred <= 7; pred++ {
			const w, h, prec = 37, 11, 14
			samples := make([][]uint16, comps)
			for c := range samples {
				samples[c] = make([]uint16, w*h)
				for i := range samples[c] {
					switch rng.Intn(4) {
					case 0:
						samples[c][i] = 16383 // saturated
					case 1:
						samples[c][i] = 0
					default:
						samples[c][i] = uint16(rng.Intn(16384))
					}
				}
			}
			data := encodeLJPEG(samples, w, h, comps, prec, pred)
			rows := 0
			gw, gh, gc, err := decodeLJPEG(data, func(y int, row []uint16) {
				rows++
				for x := 0; x < w; x++ {
					for c := 0; c < comps; c++ {
						if got, want := row[x*comps+c], samples[c][y*w+x]; got != want {
							t.Fatalf("comps %d pred %d: (%d,%d,c%d) = %d, want %d", comps, pred, x, y, c, got, want)
						}
					}
				}
			})
			if err != nil || gw != w || gh != h || gc != comps || rows != h {
				t.Fatalf("comps %d pred %d: %dx%d c%d rows %d err %v", comps, pred, gw, gh, gc, rows, err)
			}
		}
	}
}

func TestLJPEGRejectsGarbage(t *testing.T) {
	if _, _, _, err := decodeLJPEG([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0, 2}, func(int, []uint16) {}); err == nil {
		t.Fatal("baseline/garbage accepted")
	}
	if _, _, _, err := decodeLJPEG(nil, func(int, []uint16) {}); !errors.Is(err, errNotLJPEG) {
		t.Fatalf("empty input: %v", err)
	}
}

// withDHT replaces the encoder's Huffman table (bits[0..15] + symbols).
func withDHT(data []byte, bits [16]byte, syms []byte) []byte {
	// SOI(2) + FFC4(2) + length(2) + class/id(1) + 16 + 17 symbols from encodeLJPEG
	rest := data[2+2+2+1+16+17:]
	out := append([]byte{}, data[:4]...)
	n := 2 + 1 + 16 + len(syms)
	out = append(out, byte(n>>8), byte(n), 0x00)
	out = append(out, bits[:]...)
	out = append(out, syms...)
	return append(out, rest...)
}

func TestLJPEGRejectsMalformedTablesWithoutPanicking(t *testing.T) {
	good := encodeLJPEG([][]uint16{make([]uint16, 16)}, 4, 4, 1, 14, 1)
	var bits [16]byte
	bits[0] = 3 // three 1-bit codes: over-subscribed
	syms := []byte{0, 1, 2}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("over-subscribed table panicked: %v", r)
			}
		}()
		if _, _, _, err := decodeLJPEG(withDHT(good, bits, syms), func(int, []uint16) {}); err == nil {
			t.Fatal("over-subscribed Huffman table accepted")
		}
	}()
	var ok [16]byte
	ok[4] = 17
	cats := make([]byte, 17)
	for i := range cats {
		cats[i] = byte(i)
	}
	cats[0] = 40 // code 00000 now means category 40: not a valid difference category
	if _, _, _, err := decodeLJPEG(withDHT(good, ok, cats), func(int, []uint16) {}); err == nil || !strings.Contains(err.Error(), "category") {
		t.Fatalf("difference category 40: %v", err)
	}
}

func TestMeasureRecoversFromDecoderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Measure panicked: %v", r)
		}
	}()
	good := encodeLJPEG([][]uint16{make([]uint16, 16)}, 4, 4, 1, 14, 1)
	var bits [16]byte
	bits[0] = 3
	if _, err := Measure(rawDNG(t, withDHT(good, bits, []byte{0, 1, 2}), 4, 4)); err == nil {
		t.Fatal("malformed raw accepted")
	}
}
