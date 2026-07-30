// Package video — DCT block decode for Avocent codec (opcodes 0, 4, 8, 12).
//
// This uses the same bit buffer as the Avocent state machine (nb/mb/ob).
// It's called from AvocentDecoder.dispatchDCT. The bit consumption format
// is JPEG-baseline Huffman + AAN-scaled IDCT with 4-channel output
// (Y, Cb, Cr, A). Only Y/Cb/Cr are rendered; A is decoded but currently
// ignored.

package video

import "math"

// avoHuff is a length-limited Huffman table sized like com.avocent.kvm.a.a.f.
// Values are indexed by (length << 8) | offset — matching d.java line 583's
// `a(char c2, char c3) = (c2 << 8) | c3` addressing scheme.
type avoHuff struct {
	minCode [17]int32
	maxCode [17]int32
	values  [65536]byte // sparse; only positions (l<<8)|k for k in [0..bits[l]) valid
}

func newAvoHuff(bits [17]byte, values []byte) *avoHuff {
	h := &avoHuff{}
	code := int32(0)
	vi := 0
	for l := 1; l <= 16; l++ {
		if bits[l] == 0 {
			// Java d.java:616: b[c3] = 65535, c[c3] = 0 when no codes at this
			// length — a range that never matches since 65535 > 0.
			h.minCode[l] = 65535
			h.maxCode[l] = 0
		} else {
			h.minCode[l] = code
			for j := byte(0); j < bits[l]; j++ {
				h.values[(l<<8)|int(j)] = values[vi]
				vi++
				code++
			}
			h.maxCode[l] = code - 1
		}
		code <<= 1
	}
	return h
}

// avoExtend is the K[] table for JPEG-style sign extension.
var avoExtend = [16]int16{
	0, -1, -3, -7, -15, -31, -63, -127,
	-255, -511, -1023, -2047, -4095, -8191, -16383, -32767,
}

// zigZag is defined in jpeg.go: natural→zigzag map (wb[] in Java).
// invZigZag maps zigzag position → natural position — needed to place
// bitstream-order coefficients into their spatial slot per Java d.java:900
// `O[i2] = Vb[wb[i2]]` (equivalent: coef[invZigZag[k]] = Vb[k]).
var invZigZag [64]byte

func init() {
	for natural := 0; natural < 64; natural++ {
		invZigZag[zigZag[natural]] = byte(natural)
	}
}

// avoDCTables is the two DC + two AC Huffman tables baked in the codec.
var (
	avoDcLuma   *avoHuff
	avoDcChroma *avoHuff
	avoAcLuma   *avoHuff
	avoAcChroma *avoHuff
)

func init() {
	avoDcLuma = newAvoHuff(xbBits, ybVals)
	avoDcChroma = newAvoHuff(zbBits, abVals)
	avoAcLuma = newAvoHuff(bbBits, cbVals)
	avoAcChroma = newAvoHuff(dbBits, ebVals)
}

// avoQuant holds the AAN-prescaled quantisation table for one channel.
type avoQuant [64]float32

// buildAvoQuant produces the AAN-prescaled quant table.
//
// Java's d.a(float[]) at line 273: fArray[n2] = cArray[wb[n2]] — takes
// cArray in wire (zigzag) order and unzigzags into fArray. Then
// AAN-scales row/col. Since our source `base` matches the Java Fb/Hb
// arrays which ARE in wire order, we must unzigzag with `zigZag[natural]`
// to select the source entry.
func buildAvoQuant(base [64]int) avoQuant {
	// The Avocent Fb/Hb/.../Tb base tables are in NATURAL (raster) order, so
	// the quant value for natural position P is base[P] directly — NOT
	// base[zigZag[P]]. Applying an extra zigzag here mis-places every quant
	// coefficient, spreading high-frequency energy so glyph edges blur and
	// ring (verified: base[natural] reproduces the reference client's exact
	// mean/max luma 7/203, while the zigzag variant over-amplifies to 12/255).
	var out avoQuant
	for natural := 0; natural < 64; natural++ {
		row := natural / 8
		col := natural % 8
		out[natural] = float32(float64(base[natural]) * aanScale[row] * aanScale[col])
	}
	return out
}

// avoCos table precomputed once.
var avoCos [8][8]float32

func init() {
	for n := 0; n < 8; n++ {
		for k := 0; k < 8; k++ {
			avoCos[n][k] = float32(math.Cos(float64(2*n+1) * float64(k) * math.Pi / 16.0))
		}
	}
}

// peekBits returns the next n bits without consuming.
func (d *AvocentDecoder) peekBits(n int) uint32 {
	if n <= 0 {
		return 0
	}
	return d.nb >> (32 - n)
}

// getBits consumes n bits and returns them as MSB-aligned uint32.
func (d *AvocentDecoder) getBits(n int) uint32 {
	v := d.peekBits(n)
	d.consume(n)
	return v
}

// decodeHuffSymbol reads a variable-length Huffman code from the bit
// stream using tab and returns the value. Returns (0xFF, false) when
// no code matches within 16 bits — caller must advance position without
// treating this as EOB (matches d.java line 895 `if (n4 <= 16) continue`).
func (d *AvocentDecoder) decodeHuffSymbol(tab *avoHuff) (byte, bool) {
	for l := 1; l <= 16; l++ {
		code := int32(d.peekBits(l))
		if code >= tab.minCode[l] && code <= tab.maxCode[l] {
			d.consume(l)
			return tab.values[(l<<8)|int(code-tab.minCode[l])], true
		}
	}
	// no match — do NOT consume; caller handles
	return 0, false
}

// decodeSignedValue reads n bits and applies JPEG-style sign extension.
func (d *AvocentDecoder) decodeSignedValue(n int) int16 {
	if n == 0 {
		return 0
	}
	v := d.getBits(n)
	if v < uint32(1<<(n-1)) {
		return int16(int32(v) + int32(avoExtend[n]))
	}
	return int16(v)
}

// decodeDCTBlock decodes one 8x8 DCT block, updates DC predictor,
// dequantises via q, IDCTs, and writes 64 pixel-domain values to out.
// decodeDCTBlock decodes one 8x8 DCT block.
func (d *AvocentDecoder) decodeDCTBlock(dcTab, acTab *avoHuff, q *avoQuant, prevDC *int16, out *[64]int32) {
	var coef [64]int32
	bitsStart := d.consumedB
	sDC, ok := d.decodeHuffSymbol(dcTab)
	if ok {
		dcVal := d.decodeSignedValue(int(sDC))
		*prevDC += dcVal
	}
	// Java d.java:895 no-match path: DC coefficient stays at previous predictor
	// (sArray[0]), and AC decoding proceeds regardless. No bits consumed on
	// no-match — the outer AC loop reads from the same offset.
	coef[0] = int32(*prevDC)
	kMax := 0
	nAC := 0
	k := 1
	eob := false
	for k < 64 && !eob {
		rs, ok := d.decodeHuffSymbol(acTab)
		if !ok {
			// Java d.java:895-896: on no-match, n4=17, so ++n7 (skip 1),
			// outer while continues. No bits consumed.
			k++
			continue
		}
		s := int(rs & 0x0F)
		r := int(rs >> 4)
		if s == 0 {
			if r == 0 {
				eob = true // matches java's bl2=true
				break
			}
			if r == 15 {
				k += 16 // ZRL
				continue
			}
			// r in [1..14] with s=0 — Java d.java:883 breaks the inner for
			// but the outer while continues at the same k. So the symbol
			// has been consumed but no coefficient is written. Just loop.
			continue
		}
		k += r
		if k >= 64 {
			break
		}
		// k is the zigzag position; place into natural position via inverse map.
		coef[invZigZag[k]] = int32(d.decodeSignedValue(s))
		nAC++
		kMax = k
		k++
	}
	bitsEnd := d.consumedB
	if dctBlockDebug != nil {
		dctBlockDebug("block: sDC=%d prevDC=%d nAC=%d kMax=%d bits=%d",
			sDC, *prevDC, nAC, kMax, bitsEnd-bitsStart)
	}
	// Integer AAN IDCT with fused dequant — matches the AST firmware exactly
	// (com.avocent.kvm.a.a.d.a: multiply b(n,m)=n*m>>8, constants 362/473/277/
	// 669, final >>3 descale). A naive float cosine IDCT is NOT equivalent and
	// leaves high-frequency ringing (speckle) on sharp text edges.
	avoIDCTInt(&coef, q, out)
}

// avoIDCTInt performs the AST firmware's integer AAN IDCT with dequantisation
// folded into the first (column) pass. coef holds raw integer coefficients in
// natural order; q is the AAN-prescaled quant table (same order). Output is
// the >>3-descaled pixel-domain values WITHOUT the +128 level shift (callers
// add 128). Faithful port of d.java's a(short[], char[], char).
func avoIDCTInt(coef *[64]int32, q *avoQuant, out *[64]int32) {
	var work [64]int32
	// Column pass: for each of the 8 columns, dequant + 1-D AAN IDCT.
	for col := 0; col < 8; col++ {
		// DC-only fast path: if all 7 AC terms in the column are zero, the
		// output column is a constant = dequantised DC.
		if coef[col+8] == 0 && coef[col+16] == 0 && coef[col+24] == 0 &&
			coef[col+32] == 0 && coef[col+40] == 0 && coef[col+48] == 0 && coef[col+56] == 0 {
			dc := int32(float32(coef[col]) * q[col])
			for i := 0; i < 8; i++ {
				work[col+i*8] = dc
			}
			continue
		}
		s0 := int32(float32(coef[col+0]) * q[col+0])
		s2 := int32(float32(coef[col+16]) * q[col+16])
		s4 := int32(float32(coef[col+32]) * q[col+32])
		s6 := int32(float32(coef[col+48]) * q[col+48])
		t13 := s0 + s4
		t12 := s0 - s4
		t11 := s2 + s6
		t10 := aanMul(s2-s6, 362) - t11
		a17 := t13 + t11
		a14 := t13 - t11
		a16 := t12 + t10
		a15 := t12 - t10
		s1 := int32(float32(coef[col+8]) * q[col+8])
		s3 := int32(float32(coef[col+24]) * q[col+24])
		s5 := int32(float32(coef[col+40]) * q[col+40])
		s7 := int32(float32(coef[col+56]) * q[col+56])
		p5 := s5 + s3
		p4 := s5 - s3
		p3 := s1 + s7
		p2 := s1 - s7
		b6 := p3 + p5
		b12 := aanMul(p3-p5, 362)
		bn := aanMul(p4+p2, 473)
		b13 := aanMul(p2, 277) - bn
		b10 := aanMul(p4, -669) + bn
		b7 := b10 - b6
		b8 := b12 - b7
		b9 := b13 + b8
		work[col+8*0] = a17 + b6
		work[col+8*7] = a17 - b6
		work[col+8*1] = a16 + b7
		work[col+8*6] = a16 - b7
		work[col+8*2] = a15 + b8
		work[col+8*5] = a15 - b8
		work[col+8*4] = a14 + b9
		work[col+8*3] = a14 - b9
	}
	// Row pass: 1-D AAN IDCT per row, then >>3 descale.
	for row := 0; row < 8; row++ {
		o := row * 8
		t13 := work[o+0] + work[o+4]
		t12 := work[o+0] - work[o+4]
		t11 := work[o+2] + work[o+6]
		t10 := aanMul(work[o+2]-work[o+6], 362) - t11
		a17 := t13 + t11
		a14 := t13 - t11
		a16 := t12 + t10
		a15 := t12 - t10
		p5 := work[o+5] + work[o+3]
		p4 := work[o+5] - work[o+3]
		p3 := work[o+1] + work[o+7]
		p2 := work[o+1] - work[o+7]
		b6 := p3 + p5
		b12 := aanMul(p3-p5, 362)
		bn := aanMul(p4+p2, 473)
		b13 := aanMul(p2, 277) - bn
		b10 := aanMul(p4, -669) + bn
		b7 := b10 - b6
		b8 := b12 - b7
		b9 := b13 + b8
		out[o+0] = (a17 + b6) >> 3
		out[o+7] = (a17 - b6) >> 3
		out[o+1] = (a16 + b7) >> 3
		out[o+6] = (a16 - b7) >> 3
		out[o+2] = (a15 + b8) >> 3
		out[o+5] = (a15 - b8) >> 3
		out[o+4] = (a14 + b9) >> 3
		out[o+3] = (a14 - b9) >> 3
	}
}

// aanMul is the firmware's fixed-point multiply b(n,m) = n*m >> 8.
func aanMul(n, m int32) int32 { return (n * m) >> 8 }

var dctBlockDebug func(format string, args ...any)

func avoIDCT2D(in *[64]float32, out *[64]int32) {
	var t [64]float32
	for row := 0; row < 8; row++ {
		s := row * 8
		var y [8]float32
		avoIDCT1D(in[s:s+8], &y)
		copy(t[s:s+8], y[:])
	}
	for col := 0; col < 8; col++ {
		var v [8]float32
		for i := 0; i < 8; i++ {
			v[i] = t[i*8+col]
		}
		var u [8]float32
		avoIDCT1D(v[:], &u)
		for i := 0; i < 8; i++ {
			out[i*8+col] = int32(u[i])
		}
	}
}

func avoIDCT1D(in []float32, out *[8]float32) {
	for n := 0; n < 8; n++ {
		var sum float32
		for k := 0; k < 8; k++ {
			c := float32(1.0)
			if k == 0 {
				c = 0.7071067811865476
			}
			sum += c * in[k] * avoCos[n][k]
		}
		out[n] = sum * 0.5
	}
}

// clampInt32 clamps v to [lo, hi].
func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// dctPostShift replicates java's qb[384 + val>>3] level-shift+clamp: >>3
// on the IDCT output, +128 level shift, saturate to [0,255]. We integrate
// this into the render step.
func dctPostShift(v int32) uint32 {
	shifted := (v >> 3) + 128
	if shifted < 0 {
		return 0
	}
	if shifted > 255 {
		return 255
	}
	return uint32(shifted)
}

// dispatchDCT decodes and renders one DCT tile at (jb*q, kb*q).
//
// From d.java line 995-1019 (careful re-read of block2/block3 goto):
//
//	ab == 0 → 3 blocks per MCU (1 Y + 1 Cb + 1 Cr), 8x8 pixel tile
//	ab == 1 → 6 blocks per MCU (4 Y in 2x2 + Cb + Cr), 16x16 pixel tile
//
// q = 8 for ab==0, 16 for ab==1 (a.java:114).
func (d *AvocentDecoder) dispatchDCT(fb *Framebuffer, mode byte, qY, qC *avoQuant) {
	if dctStubMode {
		return
	}

	if mode == 0 {
		// 1 Y + 1 Cb + 1 Cr, 8x8 tile
		var yb, cb, cr [64]int32
		d.decodeDCTBlock(avoDcLuma, avoAcLuma, qY, &d.dcY, &yb)
		d.decodeDCTBlock(avoDcChroma, avoAcChroma, qC, &d.dcCb, &cb)
		d.decodeDCTBlock(avoDcChroma, avoAcChroma, qC, &d.dcCr, &cr)
		x0 := d.tileX + d.jb*8
		y0 := d.tileY + d.kb*8
		for by := 0; by < 8; by++ {
			for bx := 0; bx < 8; bx++ {
				px := x0 + bx
				py := y0 + by
				if px < 0 || px >= fb.W || py < 0 || py >= fb.H {
					continue
				}
				fb.Pixels[py*fb.W+px] = yCbCrToARGB(
					clampInt32(yb[by*8+bx]+128, 0, 255),
					clampInt32(cb[by*8+bx]+128, 0, 255),
					clampInt32(cr[by*8+bx]+128, 0, 255),
				)
			}
		}
		fb.Dirty = true
		return
	}

	// mode 1: 4 Y in 2x2 grid + Cb + Cr, 16x16 tile
	var yBlocks [4][64]int32
	var cb, cr [64]int32
	for i := 0; i < 4; i++ {
		d.decodeDCTBlock(avoDcLuma, avoAcLuma, qY, &d.dcY, &yBlocks[i])
	}
	d.decodeDCTBlock(avoDcChroma, avoAcChroma, qC, &d.dcCb, &cb)
	d.decodeDCTBlock(avoDcChroma, avoAcChroma, qC, &d.dcCr, &cr)
	x0 := d.tileX + d.jb*16
	y0 := d.tileY + d.kb*16
	for by := 0; by < 16; by++ {
		for bx := 0; bx < 16; bx++ {
			px := x0 + bx
			py := y0 + by
			if px < 0 || px >= fb.W || py < 0 || py >= fb.H {
				continue
			}
			yblk := (by/8)*2 + (bx / 8)
			fb.Pixels[py*fb.W+px] = yCbCrToARGB(
				clampInt32(yBlocks[yblk][(by%8)*8+(bx%8)]+128, 0, 255),
				clampInt32(cb[(by/2)*8+(bx/2)]+128, 0, 255),
				clampInt32(cr[(by/2)*8+(bx/2)]+128, 0, 255),
			)
		}
	}
	fb.Dirty = true
}
