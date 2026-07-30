// Package video — Avocent custom video codec (packet id=134/34310).
//
// This is NOT baseline JPEG. Rather, each tile is a stream of 4-bit
// opcodes at the top of a 32-bit bit buffer. Opcodes:
//
//	0    = full DCT-encoded 8×8 block
//	4    = DCT block with variant flag (mode 2)
//	5(u) = palette-cache 1-bit refresh, 1 cache slot
//	6(v) = palette-cache 1-bit refresh, 2 cache slots
//	7(w) = palette-cache 1-bit refresh, 4 cache slots
//	8(s) = tile position update, 1-slot palette-cache refresh
//	9(t) = END-OF-STREAM
//	12(r)= reserved (empty; identity)
//	13(x)= tile position update, 2-slot palette-cache refresh
//	14(y)= tile position update, 4-slot palette-cache refresh
//	15   = tile position update, 4-slot palette-cache refresh + jump
//
// Each 8×8 tile is drawn from a "palette cache" of up to 4 recent
// 24-bit RGB colours (vb.a[]). Opcodes 5..15 provide fresh colours and
// index bits; the render step picks one of the cached colours per pixel
// using vb.c-many bits per pixel.
//
// The DCT opcodes (0, 4) use YCbCr-encoded blocks similar to JPEG. This
// file implements the palette-cache subsystem first so non-DCT tiles
// render. DCT is a follow-up (currently produces black for those tiles).
package video

// paletteCache is the vb structure in d.java: 4 recent 24-bit RGB
// colours plus lookup indices for the current block.
type paletteCache struct {
	colors  [4]uint32 // vb.a[0..3]
	index   [4]int    // vb.b[0..3]
	entropy int       // vb.c: bits-per-pixel (0..2)
}

// AvocentDecoder is a stateful decoder for the packet-134 codec.
type AvocentDecoder struct {
	// bit buffer: top 32 bits of nb hold the current state; new bits
	// stream in as opcodes consume them
	nb        uint32
	mb        uint32
	ob        int // valid bits remaining in mb (top-aligned); nb is always fully populated
	src       []byte
	pos       int
	consumedB int // total bits consumed via consume() since Decode() start

	// palette cache
	vb paletteCache

	// current tile position within the destination framebuffer
	jb, kb int

	// tile origin in destination framebuffer, set from ib packet header
	// (bytes 1..3 packed 24-bit: hi12=x, lo12=y)
	tileX, tileY int

	// tile dimensions in pixels, from ib packet header bytes 4..7 (height,
	// width). The MCU raster (jb/kb) wraps within these bounds — NOT the full
	// framebuffer. Java d.a(width,height) sets bb=width, cb=height and e()
	// wraps jb at bb/q, kb at cb/q. Using the framebuffer size instead smears
	// dirty-rectangle tiles horizontally (ghosting).
	tileW, tileH int

	// tile-size multiplier (8 for 4:4:4, 16 for 4:2:2). Comes from `q`
	// in a.java line 114.
	q int

	// palette-cache index cache from opcode-13 onwards
	vbCPrev int

	// DCT predictor state
	dcY, dcCb, dcCr int16
	// mode set from ib packet header byte 11's nibble; 0=4:4:4, 1=4:2:2
	mode byte
	// quantisation tables per channel — start with quality 3
	qY, qC avoQuant

	// Debug: opcode counting
	lastOpCount int
	opTrail     []int

	// Debug: force a specific quant-table index (>=0 overrides packet value).
	forceY, forceC int

	// Debug: opcode hook (nil in production).
	opHook func(op int)
}

// NewAvocentDecoder returns a decoder ready for a fresh stream.
func NewAvocentDecoder() *AvocentDecoder {
	d := &AvocentDecoder{q: 8, forceY: -1, forceC: -1}
	d.qY = buildAvoQuant(quantY[7])
	d.qC = buildAvoQuant(quantC[7])
	return d
}

// SetMode configures the codec state from the ib packet header.
// From a.a.a.java line 114: q = (ab == 1 ? 16 : 8).
// From d.java line 995-1019 & 918-985 (careful re-read):
//
//	ab == 0 → 3 blocks per MCU (1 Y + Cb + Cr), 8x8 pixel tile
//	ab == 1 → 6 blocks per MCU (4 Y + Cb + Cr), 16x16 pixel tile
func (d *AvocentDecoder) SetMode(mode, lumaQ, chromaQ byte) {
	d.mode = mode
	if mode == 1 {
		d.q = 16
	} else {
		d.q = 8
	}
	// Quant-table index comes from the packet's byte-11 nibbles (lumaQ=q hi,
	// chromaQ=p lo) but is REVERSED vs our table order: firmware maps W=0→Tb
	// (coarsest) … W=7→Fb (finest); our quantY/quantC are finest-first, so use
	// index 7-v. (Combined with the base[natural] quant fix this reproduces the
	// reference client's crisp output; the earlier "fixed idx7" workaround was
	// only masking the quant-ordering bug.)
	yi := 7 - int(lumaQ)
	ci := 7 - int(chromaQ)
	if d.forceY >= 0 {
		yi = d.forceY
	}
	if d.forceC >= 0 {
		ci = d.forceC
	}
	if yi < 0 || yi >= len(quantY) {
		yi = 7
	}
	if ci < 0 || ci >= len(quantC) {
		ci = 7
	}
	d.qY = buildAvoQuant(quantY[yi])
	d.qC = buildAvoQuant(quantC[ci])
}

// opCount counts opcodes processed per Decode call (for debugging).
func (d *AvocentDecoder) OpCount() int { return d.lastOpCount }

// LastOpTrail returns the last few opcodes decoded (for debugging).
func (d *AvocentDecoder) LastOpTrail() []int { return d.opTrail }

// PosBytes returns how many bytes have been read from src.
func (d *AvocentDecoder) PosBytes() int { return d.pos }

// BufBits returns how many bits are still buffered (not yet consumed).
// With the split nb/mb layout, that's 32 (nb) + ob (mb) once initialized.
func (d *AvocentDecoder) BufBits() int { return 32 + d.ob }

// DecodePacket accepts the raw ib packet payload (starting at byte 0 of
// f.Payload where byte 0 is the packet subtype `o`). It parses the ib
// header (matching ib.setData + t.java line 142) and, if the subtype
// selects the DCT codec, invokes the entropy decoder. Returns the number
// of bytes consumed from the body (>= 0) or -1 on frame error.
//
// Header layout (from ib.setData):
//
//	[0]     subtype (o)
//	[1..3]  packed 24-bit: r = hi12 (tile x), s = lo12 (tile y)
//	[4..5]  m (short BE) — height
//	[6..7]  n (short BE) — width
//	[8]     flags: bit0=k, bit1=j, bit2=l, bit3=w, bit4=x
//	[9..10] short BE: t = hi>>10 & 0x3F (lumaQ),
//	                 u = hi>>4 & 0x3F (chromaQ), v = & 0xF (mode)
//	[11]    p = lo nibble, q = hi nibble (palette-cache init)
//	[12..]  compressed body
func (d *AvocentDecoder) DecodePacket(fb *Framebuffer, payload []byte) int {
	if len(payload) < 12 {
		return -1
	}
	subtype := int(payload[0])
	tilePack := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	tileX := (tilePack >> 12) & 0xFFF
	tileY := tilePack & 0xFFF
	height := int(payload[4])<<8 | int(payload[5])
	width := int(payload[6])<<8 | int(payload[7])
	flags := payload[8]
	flagK := flags&1 != 0
	_ = flagK
	// Byte 11: p = lo nibble (Q table hi), q = hi nibble (Q table lo).
	// Java d.a(subtype, q_hi, q_lo) at d.java:198 stores ab = subtype & 1
	// (= mode) and W/X = the Q table indices from byte 11.
	pLo := byte(payload[11] & 0x0F)        // p → passed as q_lo in Java
	qHi := byte((payload[11] >> 4) & 0x0F) // q → passed as q_hi in Java
	mode := byte(subtype & 1)

	// Subtype 2/3 are "palette-only" tiles. In the reference client these are
	// stored in a tile cache (com.avocent.kvm.a.a.b, keyed by an outer frame ID)
	// and replayed later by a 0/1 reference; this BMC never emits them (its
	// stream is all subtype 5 → mode 1). Rather than silently drop them, decode
	// their palette content in place at the tile's own coordinates via the same
	// opcode machinery — the packet carries a valid tileX/tileY, so this renders
	// the tile instead of losing it. mode = subtype&1 as for 0/1.

	// Configure quant tables & mode. Note argument ordering matches the
	// Java d.a(int subtype, int q_hi, int q_lo) → SetMode(mode, lumaQ=q_hi, chromaQ=q_lo).
	d.SetMode(mode, qHi, pLo)

	body := payload[12:]
	// Java: n7 /= 4 → body length rounded down to multiple of 4.
	bodyLen := (len(body) / 4) * 4
	if bodyLen == 0 {
		return 0
	}
	body = body[:bodyLen]

	// Set tile origin as MCU coordinate (a.java line 89-91: o.a(n2,n3) then
	// jb=0, kb=0 — meaning the tile-relative MCU grid starts at 0,0. But
	// tileX/tileY tell us where inside the framebuffer this tile lives.)
	// We paint at (tileX + jb*q, tileY + kb*q). The MCU raster wraps within
	// this tile's width/height (bb/cb in Java), not the framebuffer.
	d.tileX = tileX
	d.tileY = tileY
	d.tileW = width
	d.tileH = height
	return d.decodeBody(fb, body)
}

// Decode (legacy signature) — retained so old callers compile. Treats
// payload as the raw body only, with tile origin (0,0). Prefer DecodePacket.
func (d *AvocentDecoder) Decode(fb *Framebuffer, x0, y0 int, payload []byte) int {
	d.tileX = x0
	d.tileY = y0
	return d.decodeBody(fb, payload)
}

// decodeBody runs the opcode loop over the compressed body. Called from
// DecodePacket after the ib header is parsed.
func (d *AvocentDecoder) decodeBody(fb *Framebuffer, payload []byte) int {
	d.src = payload
	d.pos = 0
	d.nb = 0
	d.mb = 0
	d.ob = 0
	d.jb = 0
	d.kb = 0
	d.dcY = 0
	d.dcCb = 0
	d.dcCr = 0
	d.lastOpCount = 0
	d.opTrail = d.opTrail[:0]
	d.consumedB = 0
	d.fill()

	for {
		// Terminate when we've consumed every bit of the source stream.
		if d.consumedB >= len(d.src)*8 {
			return d.pos
		}
		// Peek top 4 bits as opcode.
		op := int(d.nb >> 28)
		d.lastOpCount++
		if d.opHook != nil {
			d.opHook(op)
		}
		if len(d.opTrail) < 20 {
			d.opTrail = append(d.opTrail, op)
		}
		switch op {
		case 9: // END (t) — halt frame
			return d.pos
		case 0: // DCT block, mode-char 0
			d.consume(4)
			d.dispatchDCT(fb, d.mode, &d.qY, &d.qC)
		case 4: // A: palette-cache paint with mode-char 2 (NOT DCT)
			d.consume(4)
			d.paintPaletteBlock(fb) // best-effort: paint from cached palette
		case 8: // s: pos + DCT — jb/kb from nb bits [27..20],[19..12]
			d.jb = int(d.nb>>20) & 0xFF
			d.kb = int(d.nb>>12) & 0xFF
			d.consume(20)
			d.dispatchDCT(fb, d.mode, &d.qY, &d.qC)
		case 12: // r: pos + palette-cache paint with mode-char 2 (NOT DCT)
			d.jb = int(d.nb>>20) & 0xFF
			d.kb = int(d.nb>>12) & 0xFF
			d.consume(20)
			d.paintPaletteBlock(fb)
		case 5: // u: palette 1-slot refresh, entropy=0
			d.opPaletteRefresh(fb, 1, 0, false)
		case 6: // v: palette 2-slot refresh, entropy=1
			d.opPaletteRefresh(fb, 2, 1, false)
		case 7: // w: palette 4-slot refresh, entropy=2
			d.opPaletteRefresh(fb, 4, 2, false)
		case 13: // x: pos + 1-slot, entropy=0
			d.opPaletteRefresh(fb, 1, 0, true)
		case 14: // y: pos + 2-slot, entropy=1
			d.opPaletteRefresh(fb, 2, 1, true)
		case 15: // z: pos + 4-slot, entropy=2
			d.opPaletteRefresh(fb, 4, 2, true)
		default:
			// Java: n3=0, bl=true, returns -1 (frame error, halt)
			return d.pos
		}
		// Advance position after each MCU (java: this.o.e() at a.java:414)
		d.advancePos(fb.W, fb.H)
	}
}

// advancePos advances jb by 1; if it hits (tileW/q), wrap to the next MCU row.
// Matches d.java e() (line 662-670): jb wraps at bb/q, kb at cb/q, where
// bb/cb are the TILE width/height — not the framebuffer. Wrapping at the
// framebuffer width smears dirty-rectangle tiles horizontally (ghosting).
// Falls back to the framebuffer size if the tile dims are missing/zero.
func (d *AvocentDecoder) advancePos(fbW, fbH int) {
	wCols := d.tileW / d.q
	hRows := d.tileH / d.q
	if wCols <= 0 {
		wCols = fbW / d.q
	}
	if hRows <= 0 {
		hRows = fbH / d.q
	}
	d.jb++
	if d.jb >= wCols {
		d.jb = 0
		d.kb++
		if d.kb >= hRows {
			d.kb = 0
		}
	}
}

// opPaletteRefresh implements the palette-cache refresh opcodes 5..15.
// nSlots is 1/2/4; entropy is 0/1/2 bits per pixel in the tile that follows.
// If updatePos, the 20-bit consume includes opcode(4) + jb(8) + kb(8);
// otherwise just consume the 4-bit opcode.
func (d *AvocentDecoder) opPaletteRefresh(fb *Framebuffer, nSlots, entropy int, updatePos bool) {
	if updatePos {
		d.jb = int(d.nb>>20) & 0xFF
		d.kb = int(d.nb>>12) & 0xFF
		d.consume(20)
	} else {
		d.consume(4)
	}
	d.vb.entropy = entropy
	for i := 0; i < nSlots; i++ {
		idx := int(d.nb >> 29)
		d.vb.index[i] = idx & 3
		bit := int(d.nb>>31) & 1
		if bit == 0 {
			d.consume(3)
			continue
		}
		color := uint32((d.nb >> 5) & 0xFFFFFF)
		d.vb.colors[d.vb.index[i]] = 0xFF000000 | color
		d.consume(27)
	}
	d.paintPaletteBlock(fb)
}

// yLutBT601, crRedLut, cbBlueLut, crGreenLut, cbGreenLut match Java
// d.java line 533-556's precomputed I, E, F, G, H tables. This is ITU-R
// BT.601 with studio-swing (Y in [16, 235], C in [16, 240] centered on
// 128), matching what the BMC actually encodes.
var (
	yLutBT601  [256]int32 // I[Y]  = round(1.164 * (Y - 16))
	crRedLut   [256]int32 // E[Cr] = round(1.597656 * (Cr - 128))
	cbBlueLut  [256]int32 // F[Cb] = round(2.015625 * (Cb - 128))
	crGreenLut [256]int32 // G[Cr] = round(-0.8125 * (Cr - 128) * 65536), 16-bit fixed
	cbGreenLut [256]int32 // H[Cb] = round(-0.390625 * (Cb - 128) * 65536) + 32768, 16-bit fixed
)

func init() {
	scaleF := 65536.0
	half := int32(scaleF) >> 1
	fx := func(f float64) int32 { return int32(f*scaleF + 0.5) }
	for i := 0; i < 256; i++ {
		cCr := int32(i - 128)
		cCb := int32(i - 128)
		crRedLut[i] = (fx(1.597656) * cCr) >> 16
		cbBlueLut[i] = (fx(2.015625) * cCb) >> 16
		crGreenLut[i] = -fx(0.8125) * cCr
		cbGreenLut[i] = -fx(0.390625)*cCb + half
	}
	for i := 0; i < 256; i++ {
		y := int32(i - 16)
		yLutBT601[i] = (fx(1.164) * y) >> 16
	}
}

// yCbCrToARGB applies Java's BT.601 studio-swing YCbCr→RGB. Y/Cb/Cr in [0..255].
func yCbCrToARGB(y, cb, cr int32) uint32 {
	iy := yLutBT601[y&0xFF]
	r := iy + crRedLut[cr&0xFF]
	b := iy + cbBlueLut[cb&0xFF]
	g := iy + ((cbGreenLut[cb&0xFF] + crGreenLut[cr&0xFF]) >> 16)
	return 0xFF000000 | clampByte(r)<<16 | clampByte(g)<<8 | clampByte(b)
}

// paletteYCbCrToARGB converts a 24-bit packed [Y Cb Cr] palette value.
func paletteYCbCrToARGB(v uint32) uint32 {
	return yCbCrToARGB(int32(v>>16)&0xFF, int32(v>>8)&0xFF, int32(v)&0xFF)
}

// paintPaletteBlock paints a palette tile.
// Java d.b() line 1022-1047 builds Xb[0..2] (Y,Cb,Cr) with 64 samples,
// then calls d.a(n2,n3,Xb,nArray) which picks either the 8x8 (ab==0) or
// 16x16 (ab==1) painter. In ab==1 mode the painter tries to read Xb[3..5]
// and throws; the enclosing try/catch swallows it, so mode==1 palette
// tiles paint nothing. Match that behaviour.
func (d *AvocentDecoder) paintPaletteBlock(fb *Framebuffer) {
	bpp := d.vb.entropy
	// Always consume the per-pixel selector bits (Java loop at 1037-1044
	// runs 64 iterations regardless of subsequent paint outcome).
	var samples [64]uint32
	for i := 0; i < 64; i++ {
		if bpp == 0 {
			samples[i] = d.vb.colors[d.vb.index[0]]
			continue
		}
		v := int(d.nb >> (32 - bpp))
		if v >= 4 {
			v = 3
		}
		d.consume(bpp)
		samples[i] = d.vb.colors[d.vb.index[v]]
	}
	if d.mode == 1 {
		// 16x16 palette tile. The palette machinery only produces 64 selector
		// samples (an 8x8 grid), so each sample covers a 2x2 pixel block —
		// the natural 2x upscale. (The reference Java throws here because it
		// reuses the 6-plane DCT painter that expects Xb[3..5]; that path just
		// paints nothing. Painting the 64 samples as 2x2 blocks renders the
		// tile instead of dropping it, which is what the pixels represent.)
		x0 := d.tileX + d.jb*16
		y0 := d.tileY + d.kb*16
		for i := 0; i < 64; i++ {
			argb := paletteYCbCrToARGB(samples[i])
			bx := (i % 8) * 2
			by := (i / 8) * 2
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					px := x0 + bx + dx
					py := y0 + by + dy
					if px < 0 || px >= fb.W || py < 0 || py >= fb.H {
						continue
					}
					fb.Pixels[py*fb.W+px] = argb
				}
			}
		}
		fb.Dirty = true
		return
	}
	x0 := d.tileX + d.jb*8
	y0 := d.tileY + d.kb*8
	for i := 0; i < 64; i++ {
		px := x0 + (i % 8)
		py := y0 + (i / 8)
		if px < 0 || px >= fb.W || py < 0 || py >= fb.H {
			continue
		}
		fb.Pixels[py*fb.W+px] = paletteYCbCrToARGB(samples[i])
	}
	fb.Dirty = true
}

// fill loads bytes into nb+mb until at least 32 bits are available.
// Byte order controlled by BitOrderLE (default false = BE).
// fill primes nb with the first source word if empty. mb refills are
// handled inside consume() (matching Java d.a(int) at d.java:639).
func (d *AvocentDecoder) fill() {
	if d.ob == 0 && d.pos+4 <= len(d.src) {
		// Load initial nb, then initial mb.
		d.nb = d.readWord()
		if d.pos+4 <= len(d.src) {
			d.mb = d.readWord()
			d.ob = 32
		}
	}
}

// readWord reads one 32-bit word from src at d.pos (big-endian by default)
// and advances d.pos by 4. Caller must ensure d.pos+4 <= len(d.src).
func (d *AvocentDecoder) readWord() uint32 {
	var w uint32
	if BitOrderLE {
		w = uint32(d.src[d.pos]) |
			uint32(d.src[d.pos+1])<<8 |
			uint32(d.src[d.pos+2])<<16 |
			uint32(d.src[d.pos+3])<<24
	} else {
		w = uint32(d.src[d.pos])<<24 |
			uint32(d.src[d.pos+1])<<16 |
			uint32(d.src[d.pos+2])<<8 |
			uint32(d.src[d.pos+3])
	}
	d.pos += 4
	return w
}

// BitOrderLE controls the byte-to-word packing. Java d.a(byte[], int, int)
// at d.java:213 packs bytes little-endian into a 32-bit int and reads
// bits MSB-first from that int. Set to `true` to match Java exactly.
var BitOrderLE = true

// dctStubMode: if true, dispatchDCT just consumes the standard bits and
// paints nothing. Used to isolate DCT bit-consumption bugs.
var dctStubMode bool

// consume advances the bit buffer by n bits (1 <= n <= 32). Mirrors Java
// d.a(int n2) at d.java:639 exactly. Invariant: after each call, nb holds
// the next 32 bits of the stream, and mb has `ob` valid bits at the top
// (rest zero) waiting to be shifted into nb.
func (d *AvocentDecoder) consume(n int) {
	if n <= 0 {
		return
	}
	if n > 32 {
		d.consume(32)
		d.consume(n - 32)
		return
	}
	d.consumedB += n
	if d.ob-n > 0 {
		// Path A: mb has more than n valid bits; no refill needed.
		if n == 32 {
			d.nb = d.mb
			d.mb = 0
		} else {
			d.nb = (d.nb << n) | (d.mb >> (32 - n))
			d.mb <<= n
		}
		d.ob -= n
		return
	}
	// Path B: mb has <= n valid bits; must refill from src.
	var n6 uint32
	if d.pos+4 <= len(d.src) {
		n6 = d.readWord()
	}
	// Java: nb = (nb<<n) | ((mb | (n6 >>> ob)) >>> (32-n))
	// mb  = n6 << (n - ob)
	// ob  = 32 + ob - n
	if n == 32 {
		// Special-case: shift-by-32 is UB in Go.
		if d.ob == 0 {
			d.nb = n6
		} else {
			d.nb = d.mb | (n6 >> d.ob)
		}
		if d.ob == 0 {
			d.mb = 0
		} else {
			d.mb = n6 << (32 - d.ob)
		}
		// ob stays as-is: 32 + ob - 32 = ob
	} else {
		var mixed uint32
		if d.ob == 0 {
			mixed = n6
		} else {
			mixed = d.mb | (n6 >> d.ob)
		}
		d.nb = (d.nb << n) | (mixed >> (32 - n))
		if n-d.ob == 32 {
			d.mb = 0
		} else {
			d.mb = n6 << (n - d.ob)
		}
		d.ob = 32 + d.ob - n
	}
}
