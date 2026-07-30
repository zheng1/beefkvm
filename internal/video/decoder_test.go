package video

import (
	"bytes"
	"testing"
)

// synthesise a tile that: writes 4 solid pixels then repeats them across the row.
func TestDecoderRGB555Basic(t *testing.T) {
	dec := NewDecoder(Depth15)
	fb := NewFramebuffer(8, 1)

	// two COLOR ops (each = 2 bytes: op | tail):
	// pure red   0xFF0000 → RGB555 0x7C00 → op=0xFF (0x80|0x7F) tail=0x00
	// pure green 0x00FF00 → RGB555 0x03E0 → op=0x83 tail=0xE0
	// pure blue  0x0000FF → RGB555 0x001F → op=0x80 tail=0x1F
	// white     0xFFFFFF → RGB555 0x7FFF → op=0xFF tail=0xFF
	// then REPEAT 4 to fill the rest
	stream := []byte{
		0xFF, 0x00,
		0x83, 0xE0,
		0x80, 0x1F,
		0xFF, 0xFF,
		0x20 | 4, // REPEAT 4
	}
	if _, err := dec.Decode(fb, stream); err != nil {
		t.Fatal(err)
	}
	// first pixel should be red
	if r := (fb.Pixels[0] >> 16) & 0xFF; r < 0xF8 {
		t.Errorf("pixel 0 R = 0x%02x, want ≥0xF8", r)
	}
	// pixel 3 should be white
	if fb.Pixels[3]&0xFFFFFF < 0xF8F8F8 {
		t.Errorf("pixel 3 = 0x%08x, want ~white", fb.Pixels[3])
	}
	// pixels 4..7 should all equal pixel 3 (REPEAT)
	for i := 4; i < 8; i++ {
		if fb.Pixels[i] != fb.Pixels[3] {
			t.Errorf("pixel %d = 0x%08x, expected repeat of pixel 3 0x%08x", i, fb.Pixels[i], fb.Pixels[3])
		}
	}
}

func TestDecoderRGB555LUT(t *testing.T) {
	dec := NewDecoder(Depth15)
	// index 0 → black, index 0x7FFF → white
	if dec.lut[0] != 0xFF000000 {
		t.Errorf("lut[0] = 0x%08x, want black", dec.lut[0])
	}
	if got := dec.lut[0x7FFF]; got&0xFFFFFF < 0xF8F8F8 {
		t.Errorf("lut[0x7FFF] = 0x%08x, want ~white", got)
	}
}

func TestDecoderGray7(t *testing.T) {
	dec := NewDecoder(Depth7Gray)
	fb := NewFramebuffer(2, 1)
	// COLOR 0x80 → gray = 0 (black), 0xFF → gray = 254 (near white)
	stream := []byte{0x80, 0xFF}
	dec.Decode(fb, stream)
	if fb.Pixels[0] != 0xFF000000 {
		t.Errorf("gray 0 → 0x%08x", fb.Pixels[0])
	}
	got := fb.Pixels[1] & 0xFFFFFF
	// r=g=b=0xFE
	if got != 0xFEFEFE {
		t.Errorf("gray 0x7F<<1 → 0x%08x, want 0xFEFEFE", got)
	}
}

func TestDecoderPalette7(t *testing.T) {
	dec := NewDecoder(Depth7Palette)
	fb := NewFramebuffer(2, 1)
	// index 0 (r=g=b=0) then index 124 (r=g=b=255)
	stream := []byte{0x80, 0x80 | 124}
	dec.Decode(fb, stream)
	if fb.Pixels[0] != 0xFF000000 {
		t.Errorf("palette[0] = 0x%08x, want black", fb.Pixels[0])
	}
	if fb.Pixels[1] != 0xFFFFFFFF {
		t.Errorf("palette[124] = 0x%08x, want white", fb.Pixels[1])
	}
}

func TestDecoderRGB777(t *testing.T) {
	dec := NewDecoder(Depth21)
	fb := NewFramebuffer(1, 1)
	// value 0x7F 0xFF 0xFF: r=7F<<1=FE g=FF<<1=FE b=FF<<1=FE
	stream := []byte{0xFF, 0xFF, 0xFF}
	dec.Decode(fb, stream)
	// r = 0x7F8000>>15 = 0xFF, g = 0x7F80>>7 = 0xFF, b = 0x7F<<1 = 0xFE
	if fb.Pixels[0] != 0xFFFFFFFE {
		t.Errorf("RGB777 max = 0x%08x, want 0xFFFFFFFE", fb.Pixels[0])
	}
}

func TestFramebufferRepeat(t *testing.T) {
	fb := NewFramebuffer(4, 1)
	fb.Pixels[0] = 0xFF112233
	fb.Cur = 1
	fb.repeatColor(0xFFAABBCC, 3)
	if !bytes.Equal(
		asBytes(fb.Pixels[1:]),
		asBytes([]uint32{0xFFAABBCC, 0xFFAABBCC, 0xFFAABBCC}),
	) {
		t.Errorf("repeat failed: %v", fb.Pixels)
	}
}

func asBytes(u []uint32) []byte {
	b := make([]byte, len(u)*4)
	for i, v := range u {
		b[i*4] = byte(v)
		b[i*4+1] = byte(v >> 8)
		b[i*4+2] = byte(v >> 16)
		b[i*4+3] = byte(v >> 24)
	}
	return b
}
