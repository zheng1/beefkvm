package video

// paintTiles renders G tiles into fb using the current frame X[0].
// Corresponds to u.java k()/l()/m() renderers. Because F==24 is the
// only mode we've observed on the wire, we implement that directly:
// X[0] is 3 concatenated per-tile byte planes (R, G, B), each of size
// Σ tile.N. For each tile we consume plane1Off/plane2Off/plane3Off
// bytes as R/G/B and paint the tile's rectangle.
//
// For F != 24 (F==16 typical), we treat X[0] as 3 planes of the same
// concatenated layout, using u.java a() which packs pairs (n11,n12)
// into 16-bit pixels — 5-6-5 if y flag set, else 5-5-5.
func (a *AvocentV2) paintTiles(fb *Framebuffer, packetType int) {
	if fb == nil || a.G == 0 {
		return
	}
	// Compute per-plane stride: total pixel count across all tiles.
	total := 0
	for i := 0; i < a.G; i++ {
		total += a.J[i].N
	}
	if total == 0 {
		return
	}
	x := a.X[0][:a.W[0]]
	// Planes start at 0, total, 2*total (for F==24: also 3*total which is
	// unused — mirrors Java b() 4-arg call).
	p0, p1, p2 := 0, total, 2*total
	if p2+total > len(x) {
		// Guard: if decompressed data is short, clip.
		if len(x) < 3 {
			return
		}
		// Attempt any pixels we can still paint.
	}
	for i := 0; i < a.G; i++ {
		t := a.J[i]
		if t.N <= 0 {
			continue
		}
		a.paintTileRGB(fb, t, x, p0, p1, p2)
		p0 += t.N
		p1 += t.N
		p2 += t.N
	}
	fb.Dirty = true
}

func (a *AvocentV2) paintTileRGB(fb *Framebuffer, t AvocentTileDesc, x []byte, r, g, b int) {
	if t.W <= 0 || t.H <= 0 {
		return
	}
	if a.F == 16 || (a.F != 24 && a.F != 32 && a.F != 0) {
		a.paintTile16(fb, t, x, r, g, b)
		return
	}
	// F==24 / F==32: three byte planes as R, G, B.
	pxIdx := 0
	for row := 0; row < t.H; row++ {
		yy := t.Y1 + row
		if yy < 0 || yy >= fb.H {
			pxIdx += t.W
			r += t.W
			g += t.W
			b += t.W
			continue
		}
		base := yy * fb.W
		for col := 0; col < t.W; col++ {
			xx := t.X1 + col
			if xx < 0 || xx >= fb.W {
				pxIdx++
				r++
				g++
				b++
				continue
			}
			if r >= len(x) || g >= len(x) || b >= len(x) {
				return
			}
			R := uint32(x[r])
			G := uint32(x[g])
			B := uint32(x[b])
			fb.Pixels[base+xx] = 0xFF000000 | R<<16 | G<<8 | B
			pxIdx++
			r++
			g++
			b++
		}
	}
}

// paintTile16 handles F==16 (5-6-5 if y flag) / F==15 (5-5-5).
// Mirrors u.java a(int, int, int) rendered at line 659+ — packs two
// bytes n11 (high) and n12 (low) into a 16-bit pixel then expands to 8-8-8.
func (a *AvocentV2) paintTile16(fb *Framebuffer, t AvocentTileDesc, x []byte, r, g, b int) {
	_ = b // 16-bit uses only 2 planes (r high, g low), third is unused
	for row := 0; row < t.H; row++ {
		yy := t.Y1 + row
		if yy < 0 || yy >= fb.H {
			r += t.W
			g += t.W
			continue
		}
		base := yy * fb.W
		for col := 0; col < t.W; col++ {
			xx := t.X1 + col
			if xx < 0 || xx >= fb.W || r >= len(x) || g >= len(x) {
				r++
				g++
				continue
			}
			n11 := int(x[r])
			n12 := int(x[g])
			r++
			g++
			n10 := (n11 << 8) | (n12 & 0xFF)
			var R, G, B uint32
			if a.y { // RGB555
				R = uint32((n10>>10)&0x1F) << 3
				G = uint32((n10>>5)&0x1F) << 3
				B = uint32(n10&0x1F) << 3
			} else { // RGB565
				R = uint32((n10>>11)&0x1F) << 3
				G = uint32((n10>>5)&0x3F) << 2
				B = uint32(n10&0x1F) << 3
			}
			fb.Pixels[base+xx] = 0xFF000000 | R<<16 | G<<8 | B
		}
	}
}

// paintFullFrame writes X[0] as a full-frame RGB image into fb.
// Called after handleType3 (differential update completes a full frame).
// The layout follows u.java o() renderer: 3 concatenated byte planes of
// E*D pixels each, R first then G then B.
func (a *AvocentV2) paintFullFrame(fb *Framebuffer) {
	if fb == nil || a.E <= 0 || a.D <= 0 {
		return
	}
	total := a.E * a.D
	x := a.X[0][:a.W[0]]
	if len(x) < 3*total {
		return
	}
	if fb.W != a.E || fb.H != a.D {
		fb.Resize(a.E, a.D)
	}
	for i := 0; i < total; i++ {
		R := uint32(x[i])
		G := uint32(x[total+i])
		B := uint32(x[2*total+i])
		fb.Pixels[i] = 0xFF000000 | R<<16 | G<<8 | B
	}
	fb.Dirty = true
}
