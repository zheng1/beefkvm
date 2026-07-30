// Package video — JPEG-like DCT decoder for Avocent BMC packet id=134
// (VIDEO_TILE_DCT). Ported from com.avocent.kvm.a.a.d.
//
// The Avocent codec is baseline JPEG without the JFIF marker framing:
//
//   - 4 channels (Y, Cb, Cr, A) with 4:2:2 or 4:4:4 subsampling
//   - Custom Huffman tables (jpegHuff*)
//   - 8 quality levels of quantisation, each channel picks one
//   - AAN fast IDCT with 8-bit-precision fixed-point maths
//   - YCbCr → RGB via precomputed clamp/level-shift LUT
//
// This implementation follows the Java class d.java exactly for the
// entropy + IDCT + colour path. Some Java-specific tricks (state fields,
// re-entrant multi-packet decode) are collapsed into a single Decode
// call per tile.
package video

// zigZag maps natural-order block index to zigzag-scan position.
// From com.avocent.kvm.a.a.d.wb (2049).
var zigZag = [64]byte{
	0, 1, 5, 6, 14, 15, 27, 28,
	2, 4, 7, 13, 16, 26, 29, 42,
	3, 8, 12, 17, 25, 30, 41, 43,
	9, 11, 18, 24, 31, 40, 44, 53,
	10, 19, 23, 32, 39, 45, 52, 54,
	20, 22, 33, 38, 46, 51, 55, 60,
	21, 34, 37, 47, 50, 56, 59, 61,
	35, 36, 48, 49, 57, 58, 62, 63,
}

// extendTable = K in d.java (2030). Given n bits read as value v, if the
// high bit is 0 the signed value is v + extendTable[n].
var extendTable = [16]int16{
	0, -1, -3, -7, -15, -31, -63, -127,
	-255, -511, -1023, -2047, -4095, -8191, -16383, -32767,
}

// clampLUT = qb in d.java: 1408-entry saturation table used as
// qb[384 + val>>3] to clamp IDCT output to [0,255].
var clampLUT [1408]byte

func init() {
	// c.a.a.d.b() build:
	// [0..255]      → 0
	// [256..511]    → 0..255 (identity)
	// [512..895]    → 255 (saturated)
	// [896..1279]   → 0 (post-shifted negative saturation)
	// [1280..1407]  → 0..127 (currently unused; matches Java)
	for i := 256; i < 512; i++ {
		clampLUT[i] = byte(i - 256)
	}
	for i := 512; i < 896; i++ {
		clampLUT[i] = 255
	}
	for i := 896 + 384; i < 896+384+128 && i < len(clampLUT); i++ {
		clampLUT[i] = byte(i - 896 - 384)
	}
}

// DCT quantisation tables (Fb..Ub in d.java 2058..2073). Two per quality
// level (luma + chroma), 8 quality levels, 64 entries each.
var quantY = [8][64]int{
	// Fb (2058) — highest quality
	{2, 1, 1, 2, 3, 5, 6, 7, 1, 1, 1, 2, 3, 7, 7, 6, 1, 1, 2, 3, 5, 7, 8, 7, 1, 2, 2, 3, 6, 10, 10, 7, 2, 2, 4, 7, 8, 13, 12, 9, 3, 4, 6, 8, 10, 13, 14, 11, 6, 8, 9, 10, 12, 15, 15, 12, 9, 11, 11, 12, 14, 12, 12, 12},
	// Hb (2060)
	{3, 2, 1, 3, 4, 7, 9, 11, 2, 2, 2, 3, 4, 10, 11, 10, 2, 2, 3, 4, 7, 10, 12, 10, 2, 3, 4, 5, 9, 16, 15, 11, 3, 4, 6, 10, 12, 20, 19, 14, 4, 6, 10, 12, 15, 19, 21, 17, 9, 12, 14, 16, 19, 22, 22, 18, 13, 17, 17, 18, 21, 18, 19, 18},
	// Jb (2062)
	{6, 4, 3, 6, 9, 15, 19, 22, 4, 4, 5, 7, 9, 21, 22, 20, 5, 4, 6, 9, 15, 21, 25, 21, 5, 6, 8, 10, 19, 32, 30, 23, 6, 8, 13, 21, 25, 40, 38, 28, 9, 13, 20, 24, 30, 39, 42, 34, 18, 24, 29, 32, 38, 45, 45, 37, 27, 34, 35, 36, 42, 37, 38, 37},
	// Lb (2064)
	{9, 6, 5, 9, 13, 22, 28, 34, 6, 6, 7, 10, 14, 32, 33, 30, 7, 7, 9, 13, 22, 32, 38, 31, 7, 9, 12, 16, 28, 48, 45, 34, 10, 12, 20, 31, 38, 61, 57, 43, 13, 19, 30, 36, 45, 58, 63, 51, 27, 36, 43, 48, 57, 68, 67, 56, 40, 51, 53, 55, 63, 56, 57, 55},
	// Nb (2066)
	{11, 7, 7, 11, 17, 28, 36, 43, 8, 8, 10, 13, 18, 41, 43, 39, 10, 9, 11, 17, 28, 40, 49, 40, 10, 12, 15, 20, 36, 62, 57, 44, 12, 15, 26, 40, 48, 78, 74, 55, 17, 25, 39, 46, 58, 74, 81, 66, 35, 46, 56, 62, 74, 88, 86, 72, 51, 66, 68, 70, 80, 71, 74, 71},
	// Pb (2068)
	{14, 9, 9, 14, 21, 36, 46, 55, 10, 10, 12, 17, 23, 52, 54, 49, 12, 11, 14, 21, 36, 51, 62, 50, 12, 15, 19, 26, 46, 78, 72, 56, 16, 19, 33, 50, 61, 98, 93, 69, 21, 31, 49, 58, 73, 94, 102, 83, 44, 58, 70, 78, 93, 109, 108, 91, 65, 83, 86, 88, 101, 90, 93, 89},
	// Rb (2070)
	{17, 12, 10, 17, 26, 43, 55, 66, 13, 13, 15, 20, 28, 63, 65, 60, 15, 14, 17, 26, 43, 62, 75, 61, 15, 18, 24, 31, 55, 95, 87, 67, 19, 24, 40, 61, 74, 119, 112, 84, 26, 38, 60, 70, 88, 113, 123, 100, 53, 70, 85, 95, 112, 132, 131, 110, 78, 100, 103, 107, 122, 109, 112, 108},
	// Tb (2072)
	{20, 13, 12, 20, 30, 50, 63, 76, 15, 15, 17, 23, 32, 72, 75, 68, 17, 16, 20, 30, 50, 71, 86, 70, 17, 21, 27, 36, 63, 108, 100, 77, 22, 27, 46, 70, 85, 136, 128, 96, 30, 43, 68, 80, 101, 130, 141, 115, 61, 80, 97, 108, 128, 151, 150, 126, 90, 115, 118, 122, 140, 125, 128, 123},
}

var quantC = [8][64]int{
	// Gb (2059) — highest quality luma pair
	{3, 3, 4, 8, 18, 18, 18, 18, 3, 3, 4, 12, 18, 18, 18, 18, 4, 4, 10, 18, 18, 18, 18, 18, 8, 12, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18},
	// Ib (2061)
	{4, 5, 6, 13, 27, 27, 27, 27, 5, 5, 7, 18, 27, 27, 27, 27, 6, 7, 15, 27, 27, 27, 27, 27, 13, 18, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27},
	// Kb (2063)
	{9, 10, 13, 26, 55, 55, 55, 55, 10, 11, 14, 37, 55, 55, 55, 55, 13, 14, 31, 55, 55, 55, 55, 55, 26, 37, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55},
	// Mb (2065)
	{13, 14, 19, 38, 80, 80, 80, 80, 14, 17, 21, 53, 80, 80, 80, 80, 19, 21, 45, 80, 80, 80, 80, 80, 38, 53, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80},
	// Ob (2067)
	{18, 19, 26, 51, 108, 108, 108, 108, 19, 22, 28, 72, 108, 108, 108, 108, 26, 28, 61, 108, 108, 108, 108, 108, 51, 72, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108},
	// Qb (2069)
	{22, 24, 32, 63, 133, 133, 133, 133, 24, 28, 34, 88, 133, 133, 133, 133, 32, 34, 75, 133, 133, 133, 133, 133, 63, 88, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133},
	// Sb (2071)
	{27, 29, 39, 76, 160, 160, 160, 160, 29, 34, 42, 107, 160, 160, 160, 160, 39, 42, 91, 160, 160, 160, 160, 160, 76, 107, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160},
	// Ub (2073)
	{31, 33, 45, 88, 185, 185, 185, 185, 33, 39, 48, 123, 185, 185, 185, 185, 45, 48, 105, 185, 185, 185, 185, 185, 88, 123, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185},
}

// aanScale = AAN scaling factors from d.java 236, applied to
// quantisation tables (in unzigzagged order). The full effective scale
// for each entry is quant[i] * aanScale[row]*aanScale[col].
var aanScale = [8]float64{
	1.0, 1.387039845322148, 1.306562964876377, 1.175875602419359,
	1.0, 0.785694958387102, 0.541196100146197, 0.275899379282943,
}

// buildScaledQuant multiplies a base quantisation table (in wb-zigzag
// order in the raw constant, but wb maps natural→zigzag, so the tables
// above are already indexed in natural order after unzigzag). Returns
// the AAN-prescaled 64-entry table.
func buildScaledQuant(base [64]int) [64]float32 {
	var out [64]float32
	for zz := 0; zz < 64; zz++ {
		natural := int(zigZag[zz])
		row := natural / 8
		col := natural % 8
		s := aanScale[row] * aanScale[col]
		out[natural] = float32(float64(base[zz]) * s)
	}
	return out
}

// huffmanTable is a length-limited Huffman decode structure matching the
// Java com.avocent.kvm.a.a.f fields b/c/d (minCode, maxCode, values).
type huffmanTable struct {
	minCode [17]int    // per code length; 0xFFFF marks "no code of this length"
	maxCode [17]int    // per code length; -1 marks "no code"
	values  [4096]byte // linearised value table indexed by
	valPtr  [17]int    // start offset into values[] per code length
}

// buildHuffman constructs a table from JPEG-style bits + huffvals.
// bits[i] = count of codes with i bits (1..16). huffvals lists the
// symbols in code order.
func buildHuffman(bits [17]byte, huffvals []byte) *huffmanTable {
	h := &huffmanTable{}
	code := 0
	vi := 0
	for l := 1; l <= 16; l++ {
		if bits[l] == 0 {
			h.minCode[l] = 0x10000
			h.maxCode[l] = -1
			h.valPtr[l] = vi
		} else {
			h.minCode[l] = code
			h.valPtr[l] = vi
			for j := byte(0); j < bits[l]; j++ {
				h.values[vi] = huffvals[vi]
				vi++
				code++
			}
			h.maxCode[l] = code - 1
		}
		code <<= 1
	}
	return h
}

// Precomputed 4 Huffman tables (dc-luma, dc-chroma, ac-luma, ac-chroma).
// The constants below are the same ones baked into d.java 2050..2057.
var (
	dcLumaTable   *huffmanTable
	dcChromaTable *huffmanTable
	acLumaTable   *huffmanTable
	acChromaTable *huffmanTable
)

// Huffman bit counts + values from d.java static block (xb/yb, zb/Ab, Bb/Cb, Db/Eb).
var (
	xbBits = [17]byte{0, 0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}
	ybVals = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}

	zbBits = [17]byte{0, 0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
	abVals = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}

	bbBits = [17]byte{0, 0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7D}
	cbVals = []byte{
		0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
		0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
		0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
		0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
		0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
		0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
		0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
		0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
		0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}

	dbBits = [17]byte{0, 0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}
	ebVals = []byte{
		0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
		0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
		0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
		0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
		0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
		0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
		0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
		0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
		0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}
)

func init() {
	dcLumaTable = buildHuffman(xbBits, ybVals)
	dcChromaTable = buildHuffman(zbBits, abVals)
	acLumaTable = buildHuffman(bbBits, cbVals)
	acChromaTable = buildHuffman(dbBits, ebVals)
}

// bitStream provides MSB-first bit access over a byte slice. No 0xFF byte
// stuffing removal — the Avocent stream is raw.
type bitStream struct {
	data []byte
	pos  int
	acc  uint64 // bits held in high positions
	n    int    // valid bits in acc
}

func (b *bitStream) fill() {
	for b.n <= 56 && b.pos < len(b.data) {
		b.acc |= uint64(b.data[b.pos]) << (56 - b.n)
		b.n += 8
		b.pos++
	}
}

func (b *bitStream) get(k int) uint32 {
	b.fill()
	if b.n < k {
		return 0
	}
	v := uint32(b.acc >> (64 - k))
	b.acc <<= k
	b.n -= k
	return v
}

// decodeSymbol consumes a Huffman code and returns the value.
func (b *bitStream) decodeSymbol(h *huffmanTable) byte {
	code := 0
	for l := 1; l <= 16; l++ {
		code = (code << 1) | int(b.get(1))
		if code <= h.maxCode[l] {
			return h.values[h.valPtr[l]+(code-h.minCode[l])]
		}
	}
	return 0
}

// decodeValue reads n bits and applies JPEG sign-extend.
func (b *bitStream) decodeValue(n int) int16 {
	if n == 0 {
		return 0
	}
	v := b.get(n)
	if v < uint32(1<<(n-1)) {
		return int16(int(v) + int(extendTable[n]))
	}
	return int16(v)
}

// decodeBlockJPEG decodes one 8x8 block. prevDC is updated in place.
// scaledQuant is the AAN-prescaled quantisation table (natural order).
// out receives 64 pixel-domain values in natural order.
func decodeBlockJPEG(bs *bitStream, dcH, acH *huffmanTable, scaledQuant *[64]float32, prevDC *int16, out *[64]int32) {
	var coef [64]int32
	// DC
	sDC := int(bs.decodeSymbol(dcH))
	diff := bs.decodeValue(sDC)
	*prevDC += diff
	coef[0] = int32(*prevDC)
	// AC
	k := 1
	for k < 64 {
		rs := bs.decodeSymbol(acH)
		s := int(rs & 0x0F)
		r := int(rs >> 4)
		if s == 0 {
			if r == 15 {
				k += 16
				continue
			}
			break
		}
		k += r
		if k >= 64 {
			break
		}
		coef[zigZag[k]] = int32(bs.decodeValue(s))
		k++
	}
	// Multiply by dequantised AAN-prescaled table.
	var scaled [64]float32
	for i := 0; i < 64; i++ {
		scaled[i] = float32(coef[i]) * scaledQuant[i]
	}
	// 2D IDCT
	idct2D(&scaled, out)
}

// idct2D — separable AAN 8x8 IDCT.
func idct2D(in *[64]float32, out *[64]int32) {
	var t [64]float32
	// rows
	for row := 0; row < 8; row++ {
		s := row * 8
		var y [8]float32
		idct1D(in[s:s+8], &y)
		copy(t[s:s+8], y[:])
	}
	// cols
	for col := 0; col < 8; col++ {
		var v [8]float32
		for i := 0; i < 8; i++ {
			v[i] = t[i*8+col]
		}
		var u [8]float32
		idct1D(v[:], &u)
		for i := 0; i < 8; i++ {
			out[i*8+col] = int32(u[i])
		}
	}
}

// idct1D — 8-point straightforward IDCT.
func idct1D(in []float32, out *[8]float32) {
	for n := 0; n < 8; n++ {
		var sum float32
		for k := 0; k < 8; k++ {
			c := float32(1.0)
			if k == 0 {
				c = 0.7071067811865476
			}
			sum += c * in[k] * cosTab[n][k]
		}
		out[n] = sum * 0.5
	}
}

// cosTab[n][k] = cos((2n+1)*k*pi/16).
var cosTab [8][8]float32

func init() {
	const pi = 3.141592653589793
	for n := 0; n < 8; n++ {
		for k := 0; k < 8; k++ {
			cosTab[n][k] = float32(mathCos(float64(2*n+1) * float64(k) * pi / 16.0))
		}
	}
}

// clampByte clamps to [0,255] via lookup (matches d.java qb).
func clampByte(v int32) uint32 {
	// The Java code does: qb[384 + (val>>3) & 0x3FF]. Emulate that with
	// naive clamp since we're already in the pixel domain (no >>3 needed).
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint32(v)
}

// DecodeJPEGTile decodes a full JPEG-DCT tile into the framebuffer at the
// cursor position, then advances the cursor by tileW*tileH pixels along
// scanlines.
//
// Tile format (packet id 134 payload starting at offset 12):
//   - subsampling mode byte (0 = 4:4:4, 1 = 4:2:2)
//   - luma quality byte (0..7)
//   - chroma quality byte (0..7)
//   - width and height in MCU units (2 bytes each, big-endian short)
//   - entropy-coded bitstream (MCU-major, block-major within MCU)
//
// This is a functional port of com.avocent.kvm.a.a.d.a(byte[],int,int) +
// b(int,int,int[],char).
type JPEGDecoder struct {
	// Cached DC predictors per channel (Y/Cb/Cr/A).
	dc [4]int16
}

// NewJPEGDecoder returns a fresh decoder.
func NewJPEGDecoder() *JPEGDecoder { return &JPEGDecoder{} }

// Reset zeroes the DC predictors — call at start of each tile so the
// predictor doesn't leak between packets.
func (d *JPEGDecoder) Reset() { d.dc = [4]int16{} }

// DecodeTile decodes payload into fb starting at (x0, y0). tileW/H are
// pixel dimensions of the region. Returns bytes consumed.
func (d *JPEGDecoder) DecodeTile(fb *Framebuffer, x0, y0, tileW, tileH int, mode, yq, cq byte, payload []byte) (int, error) {
	if int(yq) >= len(quantY) || int(cq) >= len(quantC) {
		return 0, nil
	}
	yScaled := buildScaledQuant(quantY[yq])
	cScaled := buildScaledQuant(quantC[cq])

	bs := &bitStream{data: payload}
	d.Reset()

	mcuW := 8
	mcuH := 8
	if mode == 1 {
		mcuW = 16
	}

	mcuX := (tileW + mcuW - 1) / mcuW
	mcuY := (tileH + mcuH - 1) / mcuH

	var cbBlk, crBlk [64]int32
	nBlocks := 0
	for my := 0; my < mcuY; my++ {
		for mx := 0; mx < mcuX; mx++ {
			nY := 1
			if mode == 1 {
				nY = 2
			}
			var yBlocks [2][64]int32
			for i := 0; i < nY; i++ {
				decodeBlockJPEG(bs, dcLumaTable, acLumaTable, &yScaled, &d.dc[0], &yBlocks[i])
			}
			decodeBlockJPEG(bs, dcChromaTable, acChromaTable, &cScaled, &d.dc[1], &cbBlk)
			decodeBlockJPEG(bs, dcChromaTable, acChromaTable, &cScaled, &d.dc[2], &crBlk)
			if nBlocks == 0 && jpegDebug != nil {
				jpegDebug("first-MCU Y[0..7]=%v cb[0..3]=%v cr[0..3]=%v dc=%v",
					yBlocks[0][:8], cbBlk[:4], crBlk[:4], d.dc)
			}
			nBlocks++
			renderMCU(fb, x0+mx*mcuW, y0+my*mcuH, mode, &yBlocks, &cbBlk, &crBlk)
		}
	}
	return bs.pos, nil
}

// jpegDebug can be set by tests / callers to receive debug messages.
var jpegDebug func(format string, args ...any)

// SetJPEGDebug installs a debug printer for the JPEG decoder.
func SetJPEGDebug(f func(format string, args ...any)) { jpegDebug = f }

// renderMCU converts one MCU's Y/Cb/Cr blocks to ARGB and writes to the
// framebuffer. Level shift: Y coefficient is centred, so add 128 to
// bring to [0..255]. Chroma is centred and offset by 128 in JPEG.
func renderMCU(fb *Framebuffer, x0, y0 int, mode byte, ys *[2][64]int32, cb, cr *[64]int32) {
	blocks := 1
	if mode == 1 {
		blocks = 2
	}
	for by := 0; by < 8; by++ {
		for bx := 0; bx < 8*blocks; bx++ {
			px := x0 + bx
			py := y0 + by
			if px < 0 || px >= fb.W || py < 0 || py >= fb.H {
				continue
			}
			yBlk := ys[bx/8]
			yv := yBlk[by*8+(bx%8)] + 128
			// chroma sub-sampled: shared across 2 luma pixels in mode 1
			cx := bx / blocks
			cbV := cb[by*8+cx]
			crV := cr[by*8+cx]
			r := yv + int32(float32(crV)*1.402)
			g := yv - int32(float32(cbV)*0.344136) - int32(float32(crV)*0.714136)
			b := yv + int32(float32(cbV)*1.772)
			rc := clampByte(r)
			gc := clampByte(g)
			bc := clampByte(b)
			fb.Pixels[py*fb.W+px] = 0xFF000000 | rc<<16 | gc<<8 | bc
		}
	}
	fb.Dirty = true
}
