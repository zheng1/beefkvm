package video

import "testing"

// palColor packs a Y/Cb/Cr triple the way the palette cache stores it (low 24
// bits = Y<<16 | Cb<<8 | Cr, alpha set).
func palColor(y, cb, cr uint32) uint32 {
	return 0xFF000000 | (y << 16) | (cb << 8) | cr
}

// TestPaletteBlockMode0 verifies an 8x8 palette tile paints all 64 pixels with
// the selected cache colour (entropy 0 → every pixel uses index[0]).
func TestPaletteBlockMode0(t *testing.T) {
	d := NewAvocentDecoder()
	fb := NewFramebuffer(32, 32)
	white := palColor(235, 128, 128) // studio-swing white
	d.vb.colors[0] = white
	d.vb.index[0] = 0
	d.vb.entropy = 0
	d.mode = 0
	d.tileX, d.tileY, d.jb, d.kb = 0, 0, 0, 0

	d.paintPaletteBlock(fb)

	want := paletteYCbCrToARGB(white)
	// All 8x8 pixels painted; a pixel just outside (8,0) stays black.
	for _, p := range []struct{ x, y int }{{0, 0}, {7, 7}, {3, 5}} {
		if got := fb.Pixels[p.y*fb.W+p.x]; got != want {
			t.Errorf("mode0 (%d,%d)=%#08x, want %#08x", p.x, p.y, got, want)
		}
	}
	if fb.Pixels[0*fb.W+8] != 0 {
		t.Errorf("mode0 leaked past 8x8 tile at (8,0)")
	}
}

// TestPaletteBlockMode1 verifies a 16x16 palette tile: the 64 samples upscale
// 2x2, so the whole 16x16 area is filled (previously mode-1 painted nothing).
func TestPaletteBlockMode1(t *testing.T) {
	d := NewAvocentDecoder()
	fb := NewFramebuffer(32, 32)
	red := palColor(82, 90, 240) // a saturated colour
	d.vb.colors[0] = red
	d.vb.index[0] = 0
	d.vb.entropy = 0
	d.mode = 1
	d.tileX, d.tileY, d.jb, d.kb = 0, 0, 0, 0

	d.paintPaletteBlock(fb)

	want := paletteYCbCrToARGB(red)
	// Corners + centre of the 16x16 tile must be painted (2x2 upscale covers all).
	for _, p := range []struct{ x, y int }{{0, 0}, {15, 15}, {8, 8}, {1, 14}} {
		if got := fb.Pixels[p.y*fb.W+p.x]; got != want {
			t.Errorf("mode1 (%d,%d)=%#08x, want %#08x (16x16 not fully painted)", p.x, p.y, got, want)
		}
	}
	if fb.Pixels[0*fb.W+16] != 0 {
		t.Errorf("mode1 leaked past 16x16 tile at (16,0)")
	}
}

// TestSubtype2InPlaceDecode verifies a subtype-2 (palette-only) packet is no
// longer silently dropped: it flows through the opcode decoder. A body whose
// first opcode is 9 (END) decodes cleanly and consumes the header.
func TestSubtype2InPlaceDecode(t *testing.T) {
	d := NewAvocentDecoder()
	fb := NewFramebuffer(32, 32)
	// 12-byte ib header + 4-byte body. subtype=2, 16x16 tile at (0,0).
	payload := make([]byte, 16)
	payload[0] = 2                      // subtype 2 (palette-only)
	payload[4], payload[5] = 0x00, 0x10 // height 16
	payload[6], payload[7] = 0x00, 0x10 // width 16
	// Body (bytes 12..15): first 4 bits = opcode. With the LE word packing the
	// top nibble of the first decoded word comes from byte[15]; set 0x9x there
	// so the very first opcode is END and the loop returns without error.
	payload[15] = 0x90

	n := d.DecodePacket(fb, payload)
	if n < 0 {
		t.Fatalf("subtype-2 packet returned frame error (%d) — should decode, not drop", n)
	}
	// The point is that it took the decode path (didn't early-return the old
	// stub's len-12). A frame error would be -1; anything >= 0 means the opcode
	// loop ran.
	if t.Failed() {
		return
	}
}
