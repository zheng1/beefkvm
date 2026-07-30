package video

import (
	"testing"
)

// TestAvocentBitReader verifies the bit-stream packs bytes little-endian
// into a 32-bit word (matching Java d.a(byte[],int,int) at d.java:213)
// and consumes bits MSB-first from that word.
func TestAvocentBitReader(t *testing.T) {
	// Bytes: 0x11 0x22 0x33 0x44. Packed LE = 0x44332211.
	// Top 4 bits = 0x4.
	d := NewAvocentDecoder()
	d.src = []byte{0x11, 0x22, 0x33, 0x44, 0, 0, 0, 0}
	d.pos = 0
	d.fill()
	if d.ob != 32 {
		t.Errorf("ob = %d after fill, want 32", d.ob)
	}
	if got := d.peekBits(4); got != 0x4 {
		t.Errorf("peekBits(4) = %x, want 0x4", got)
	}
	d.consume(4)
	if got := d.peekBits(4); got != 0x4 { // next nibble of 0x44
		t.Errorf("peekBits(4) after consume(4) = %x, want 0x4", got)
	}
	d.consume(4)
	// now positioned at 0x33 (next byte down in LE order)
	if got := d.peekBits(8); got != 0x33 {
		t.Errorf("peekBits(8) = %x, want 0x33", got)
	}
}

// TestAvocentSignExtend verifies decodeSignedValue matches JPEG rules.
// Under LE packing the test byte must be placed so its bits appear at the
// MSB of the packed word.
func TestAvocentSignExtend(t *testing.T) {
	// Want bits '101' at the MSB → byte must be at src[3] (top byte of LE word).
	// LE-pack: word = src[3]<<24 | src[2]<<16 | src[1]<<8 | src[0].
	// So src[3] = 0b10100000 puts '101' at top.
	d := NewAvocentDecoder()
	d.src = []byte{0, 0, 0, 0b10100000, 0, 0, 0, 0}
	d.pos = 0
	d.fill()
	if v := d.decodeSignedValue(3); v != 5 {
		t.Errorf("decodeSignedValue(3) for 0b101 = %d, want 5", v)
	}

	// value = 0b010 with n=3: high bit 0 → 0b010 + K[3]=(-7) = 2 - 7 = -5
	d = NewAvocentDecoder()
	d.src = []byte{0, 0, 0, 0b01000000, 0, 0, 0, 0}
	d.pos = 0
	d.fill()
	if v := d.decodeSignedValue(3); v != -5 {
		t.Errorf("decodeSignedValue(3) for 0b010 = %d, want -5", v)
	}
}

// TestAvocentHuffmanDC verifies decodeHuffSymbol looks up correctly.
func TestAvocentHuffmanDC(t *testing.T) {
	// '00' at top of LE word → src[3] = 0.
	d := NewAvocentDecoder()
	d.src = []byte{0, 0, 0, 0b00000000, 0, 0, 0, 0}
	d.pos = 0
	d.fill()
	if v, ok := d.decodeHuffSymbol(avoDcLuma); !ok || v != 0 {
		t.Errorf("decodeHuffSymbol '00' = %d ok=%v, want 0", v, ok)
	}

	// '010' at top → src[3] = 0b01000000 → value 1
	d = NewAvocentDecoder()
	d.src = []byte{0, 0, 0, 0b01000000, 0, 0, 0, 0}
	d.pos = 0
	d.fill()
	if v, ok := d.decodeHuffSymbol(avoDcLuma); !ok || v != 1 {
		t.Errorf("decodeHuffSymbol '010' = %d ok=%v, want 1", v, ok)
	}
}
