// Package video decodes Avocent KVM video frames into a linear ARGB
// framebuffer that a browser can render.
//
// The wire format is a stream of variable-length opcodes operating on a
// linear pixel cursor. Each opcode carries a run length encoded as a
// 5-bit initial payload optionally followed by up to 4 continuation
// bytes sharing the same 3-bit opcode prefix. Opcode families:
//
//	0x00  NOOP N       — advance the cursor by N pixels (leave old value)
//	0x40  COPY N       — copy N pixels from the previous frame's offset
//	                     (cursor - width) forward
//	0x20  REPEAT N     — repeat the last written pixel N times
//	0x60  PATTERN 4|7  — a 4- or 7-pixel run picking between two recent
//	                     pixel values per bit
//	0x80  COLOR        — depth-specific: read one or more bytes to
//	                     produce a single pixel and write it
//
// Corresponds to com.avocent.kvm.c.b (base state machine) and
// com.avocent.kvm.c.c (15-bit RGB555 decoder). Additional bit depths
// live in c.d/e/f/g/h/i and are TODO.
package video

import (
	"fmt"
)

// Framebuffer is a linear ARGB pixel buffer with a moving cursor. Cursor
// wraps at width*height.
type Framebuffer struct {
	Pixels []uint32
	W, H   int
	Cur    int
	Dirty  bool
}

// NewFramebuffer allocates a black-filled framebuffer.
func NewFramebuffer(w, h int) *Framebuffer {
	return &Framebuffer{
		Pixels: make([]uint32, w*h),
		W:      w,
		H:      h,
	}
}

// Resize replaces the buffer if dimensions changed.
func (f *Framebuffer) Resize(w, h int) {
	if w == f.W && h == f.H && f.Pixels != nil {
		return
	}
	f.Pixels = make([]uint32, w*h)
	f.W, f.H, f.Cur = w, h, 0
	for i := range f.Pixels {
		f.Pixels[i] = 0xFF000000 // opaque black
	}
	f.Dirty = true
}

// putPixel writes one ARGB value at the current cursor and advances by 1.
func (f *Framebuffer) putPixel(c uint32) {
	if f.Cur < 0 || f.Cur >= len(f.Pixels) {
		f.Cur = 0
	}
	f.Pixels[f.Cur] = c
	f.Cur++
	f.Dirty = true
}

// copyFromPrev copies n pixels starting from src into the current cursor
// position and advances by n. Used by opcode 0x40 (COPY).
func (f *Framebuffer) copyFromPrev(src, n int) {
	if n <= 0 {
		return
	}
	if src < 0 {
		return
	}
	end := f.Cur + n
	if end > len(f.Pixels) {
		end = len(f.Pixels)
	}
	for i := f.Cur; i < end && src < len(f.Pixels); i, src = i+1, src+1 {
		f.Pixels[i] = f.Pixels[src]
	}
	f.Cur = end
	f.Dirty = true
}

// repeatColor writes n copies of c starting at the cursor.
func (f *Framebuffer) repeatColor(c uint32, n int) {
	if n <= 0 {
		return
	}
	end := f.Cur + n
	if end > len(f.Pixels) {
		end = len(f.Pixels)
	}
	for i := f.Cur; i < end; i++ {
		f.Pixels[i] = c
	}
	f.Cur = end
	f.Dirty = true
}

// writePixels appends n pixels from src at the cursor.
func (f *Framebuffer) writePixels(src []uint32) {
	n := len(src)
	end := f.Cur + n
	if end > len(f.Pixels) {
		end = len(f.Pixels)
		n = end - f.Cur
	}
	copy(f.Pixels[f.Cur:end], src[:n])
	f.Cur = end
	f.Dirty = true
}

// getPixel reads the ARGB value at absolute index i (or 0 if out of range).
func (f *Framebuffer) getPixel(i int) uint32 {
	if i < 0 || i >= len(f.Pixels) {
		return 0xFF000000
	}
	return f.Pixels[i]
}

// Decoder consumes an AVPT tile payload and mutates fb accordingly.
// The pixel-decode primitive `readColor` varies by bit depth; the tile
// stream layout is shared across depths.
type Decoder struct {
	depth Depth
	// palette lookup for depths that pre-compute RGB → ARGB tables
	lut []uint32
	// palette is the active palette for Depth7Palette. When nil, the
	// default paletteC7 is used. Overridden by frame 138 (VGA palette
	// update) via SetPalette.
	palette []uint32
}

// SetPalette overrides the decoder's palette for Depth7Palette mode.
// Called on frame 138 (VGA palette change) with 128 ARGB entries; entries
// beyond the input length preserve the previous value. Only meaningful for
// Depth7Palette decoders.
func (d *Decoder) SetPalette(entries []uint32) {
	if d.palette == nil {
		d.palette = make([]uint32, 128)
		copy(d.palette, paletteC7[:])
	}
	n := len(entries)
	if n > 128 {
		n = 128
	}
	copy(d.palette[:n], entries[:n])
}

// Depth selects which BMC video encoding this decoder handles. Values
// match the com.avocent.kvm.c.* class layout:
//
//	Depth7Gray    → c.e — 7-bit grayscale (1 byte per COLOR opcode)
//	Depth7Palette → c.f — 128-entry palette (1 byte, LUT via com.avocent.kvm.c.i)
//	Depth15       → c.c — 15-bit RGB555 (2 bytes)
//	Depth21       → c.d — 21-bit RGB777 (3 bytes)
//
// The BMC advertises the current depth in the video-tile packet header;
// the client must dispatch on that. See s.java tile handler dispatch.
type Depth int

const (
	Depth7Gray    Depth = 7
	Depth7Palette Depth = 8
	Depth15       Depth = 15
	Depth21       Depth = 21
)

// Palette used by Depth7Palette. Matches com.avocent.kvm.c.i.a[] after
// initialization: a 5×5×5 RGB colour cube (indices 0..124) plus three
// extra greys (125..127). Values are ARGB uint32.
var paletteC7 [128]uint32

func init() {
	// c[] from com.avocent.kvm.c.i: 5-step ramps at 0/64/128/192/255
	// (the Java table uses `<< 0`, `<< 8`, `<< 16` shifts, giving
	//  BGRA layout — we invert R/B to get ARGB per our convention).
	steps := [5]uint32{0, 64, 128, 192, 255}
	idx := 0
	for r := 0; r < 5; r++ {
		for g := 0; g < 5; g++ {
			for b := 0; b < 5; b++ {
				paletteC7[idx] = 0xFF000000 | steps[r]<<16 | steps[g]<<8 | steps[b]
				idx++
			}
		}
	}
	// grey ramps (matches Java hardcoded -10526881/-6316129/-2105377 which
	// in ARGB are 0xFF606060, 0xFF9F9F9F, 0xFFDFDFDF)
	paletteC7[125] = 0xFF606060
	paletteC7[126] = 0xFF9F9F9F
	paletteC7[127] = 0xFFDFDFDF
}

// NewDecoder returns a decoder for depth.
func NewDecoder(d Depth) *Decoder {
	dec := &Decoder{depth: d}
	switch d {
	case Depth15:
		dec.lut = make([]uint32, 1<<15)
		for i := 0; i < len(dec.lut); i++ {
			r := (i & 0x7C00) >> 7
			g := (i & 0x03E0) >> 2
			b := (i & 0x001F) << 3
			dec.lut[i] = 0xFF000000 | uint32(r)<<16 | uint32(g)<<8 | uint32(b)
		}
	case Depth7Gray, Depth7Palette, Depth21:
		// no LUT needed / uses paletteC7 for palette variant
	default:
		panic(fmt.Sprintf("video: depth %d not supported", d))
	}
	return dec
}

// Decode consumes tile payload data starting at pos, writing to fb starting
// at fb.Cur, until the payload is exhausted. Returns bytes consumed.
func (d *Decoder) Decode(fb *Framebuffer, data []byte) (int, error) {
	pos := 0
	for pos < len(data) {
		op := data[pos]
		pos++
		opFamily := op & 0xE0
		switch opFamily {
		case 0x00:
			// NOOP: advance cursor by run length
			run, consumed := readRunLength(op, 0x00, data[pos:])
			pos += consumed
			fb.Cur += run
			if fb.Cur > len(fb.Pixels) {
				fb.Cur = len(fb.Pixels)
			}
			fb.Dirty = true
		case 0x40:
			// COPY from previous frame at (cur - width)
			run, consumed := readRunLength(op, 0x40, data[pos:])
			pos += consumed
			src := fb.Cur - fb.W
			fb.copyFromPrev(src, run)
		case 0x20:
			// REPEAT last pixel
			run, consumed := readRunLength(op, 0x20, data[pos:])
			pos += consumed
			c := fb.getPixel(fb.Cur - 1)
			fb.repeatColor(c, run)
		case 0x60:
			// PATTERN: pick between two of the most recent distinct
			// pixels for each bit of the payload
			last := fb.getPixel(fb.Cur - 1)
			// find previous distinct pixel by scanning back
			var prev uint32 = last
			limit := fb.Cur - 1
			if limit > fb.W {
				limit = fb.W
			}
			for k := fb.Cur - 1; k >= 0 && k >= fb.Cur-1-limit; k-- {
				p := fb.getPixel(k)
				if p != last {
					prev = p
					break
				}
			}
			// first 4 pixels from opcode byte
			out := [7]uint32{}
			pick := func(bit int) uint32 {
				if op&byte(bit) != 0 {
					return prev
				}
				return last
			}
			out[0] = pick(0x08)
			out[1] = pick(0x04)
			out[2] = pick(0x02)
			out[3] = pick(0x01)
			fb.writePixels(out[:4])
			// if 0x10 set, continuation bytes each contribute 7 more pixels
			for op&0x10 != 0 {
				if pos >= len(data) {
					break
				}
				nb := data[pos]
				pos++
				out[0] = pickBit(nb, 0x40, prev, last)
				out[1] = pickBit(nb, 0x20, prev, last)
				out[2] = pickBit(nb, 0x10, prev, last)
				out[3] = pickBit(nb, 0x08, prev, last)
				out[4] = pickBit(nb, 0x04, prev, last)
				out[5] = pickBit(nb, 0x02, prev, last)
				out[6] = pickBit(nb, 0x01, prev, last)
				fb.writePixels(out[:7])
				op = nb
			}
		default:
			if op&0x80 == 0 {
				return pos, fmt.Errorf("video: unknown opcode 0x%02x at %d", op, pos-1)
			}
			// COLOR: depth-specific pixel decode
			c, consumed, err := d.readColor(op, data[pos:])
			if err != nil {
				return pos, err
			}
			pos += consumed
			fb.putPixel(c)
		}
	}
	return pos, nil
}

// readRunLength implements the shared g(op) VL decoder. The initial 5 bits
// come from the opcode byte; each continuation byte contributes 5 more
// low bits shifted up, and continues only while the top 3 bits match the
// family. Max 5 chunks (25 bits).
func readRunLength(op, family byte, tail []byte) (int, int) {
	run := int(op & 0x1F)
	consumed := 0
	shift := 5
	for consumed < 4 && consumed < len(tail) {
		b := tail[consumed]
		if b&0xE0 != family {
			break
		}
		run |= int(b&0x1F) << shift
		shift += 5
		consumed++
	}
	return run, consumed
}

func pickBit(b, mask byte, ifSet, ifClear uint32) uint32 {
	if b&mask != 0 {
		return ifSet
	}
	return ifClear
}

// readColor decodes one pixel for the current depth from the opcode byte
// plus following bytes.
func (d *Decoder) readColor(op byte, tail []byte) (uint32, int, error) {
	switch d.depth {
	case Depth7Gray:
		// c.e: gray = (op & 0x7F) << 1
		g := uint32(op&0x7F) << 1
		return 0xFF000000 | g<<16 | g<<8 | g, 0, nil
	case Depth7Palette:
		// c.f: 7-bit palette index. Prefer per-decoder palette (set by
		// frame 138) over the shared default paletteC7.
		if d.palette != nil {
			return d.palette[op&0x7F], 0, nil
		}
		return paletteC7[op&0x7F], 0, nil
	case Depth15:
		// c.c: 15-bit index (7 high bits + next byte)
		if len(tail) < 1 {
			return 0, 0, fmt.Errorf("video: truncated RGB555 color")
		}
		idx := (int(op&0x7F) << 8) | int(tail[0])
		return d.lut[idx&0x7FFF], 1, nil
	case Depth21:
		// c.d: 21-bit RGB777 packed into 3 bytes; each 7-bit channel expanded to 8-bit via <<1
		if len(tail) < 2 {
			return 0, 0, fmt.Errorf("video: truncated RGB777 color")
		}
		v := uint32(op&0x7F)<<16 | uint32(tail[0])<<8 | uint32(tail[1])
		r := (v & 0x7F8000) >> 15
		g := (v & 0x007F80) >> 7
		b := (v & 0x00007F) << 1
		return 0xFF000000 | r<<16 | g<<8 | (b & 0xFF), 2, nil
	}
	return 0, 0, fmt.Errorf("video: depth %d not supported", d.depth)
}
