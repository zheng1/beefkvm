package video

import (
	"encoding/binary"
	"fmt"
)

// handleType4/5/6: tile-list packet.
// Layout: [1]=z flags, [2]=A, [3]=B escapes,
// [4..5]=l width, [6..7]=k height,
// [8]=x=I() (initial-flag), [9]=y=J() (tile count on init packet),
// body from 12.
//
// On first packet of a burst (z&1): body starts with G*8 bytes of tile
// descriptors, then compressed RLE payload. On continuation packets:
// entire body is RLE payload.
// On z&2 (final packet): decompress accumulated S[0], then render.
func (a *AvocentV2) handleType4(p []byte, fb *Framebuffer) (bool, error) {
	if len(p) < 12 {
		return false, fmt.Errorf("avo2: type-4/5/6 short: %d", len(p))
	}
	packetType := int(p[0])
	z := p[1]
	a.A = int(p[2]) // r = F() = A marker
	a.B = int(p[3]) // s = G() = B marker
	// bytes 4-5, 6-7 = l (width), k (height) — dims for c(z, y, I)
	l := int(binary.BigEndian.Uint16(p[4:6]))
	k := int(binary.BigEndian.Uint16(p[6:8]))
	pxFmt := int(p[8])   // x = I() = pixel format (24, 16, 15, ...)
	tileCnt := int(p[9]) // y = J() = tile count on init packet
	body := p[12:]
	nBody := len(body)
	bodyOff := 0

	// z&1 => first packet of burst: reset buffer and read tile descriptor list
	if z&1 > 0 {
		a.U[0] = 0
		a.W[0] = 0
		a.G = tileCnt
		a.F = pxFmt
		a.E = l
		a.D = k
		if a.G > len(a.J) {
			a.J = make([]AvocentTileDesc, a.G)
		}
		descBytes := a.G * 8
		if descBytes > nBody {
			return false, fmt.Errorf("avo2: tile descriptors %d > body %d", descBytes, nBody)
		}
		for i := 0; i < a.G; i++ {
			// Java o.a=short0(x2), o.b=short1(x1), o.c=short2(y2), o.d=short3(y1)
			x2 := int(int16(binary.BigEndian.Uint16(body[i*8 : i*8+2])))
			x1 := int(int16(binary.BigEndian.Uint16(body[i*8+2 : i*8+4])))
			y2 := int(int16(binary.BigEndian.Uint16(body[i*8+4 : i*8+6])))
			y1 := int(int16(binary.BigEndian.Uint16(body[i*8+6 : i*8+8])))
			w := x2 - x1 + 1
			h := y2 - y1 + 1
			a.J[i] = AvocentTileDesc{X2: x2, X1: x1, Y2: y2, Y1: y1, W: w, H: h, N: w * h}
		}
		bodyOff = descBytes
		// Allocate S[0]/X[0] if needed (dimensions may not have been set by
		// a type-1 packet in some session flows)
		if a.S[0] == nil {
			if a.V < 131072 {
				a.V = 131072
			}
			a.S[0] = make([]byte, a.V)
			a.X[0] = make([]byte, a.V)
		}
	}
	// Copy remaining body into S[0]
	rem := nBody - bodyOff
	if rem > 0 {
		if a.U[0]+rem > a.V {
			// Grow buffer
			newV := a.V
			for newV < a.U[0]+rem {
				newV *= 2
			}
			ns := make([]byte, newV)
			copy(ns, a.S[0][:a.U[0]])
			a.S[0] = ns
			nx := make([]byte, newV)
			a.X[0] = nx
			a.V = newV
		}
		copy(a.S[0][a.U[0]:], body[bodyOff:])
		a.U[0] += rem
	}
	if z&2 == 0 {
		return false, nil
	}
	// Final packet: decompress and render
	if err := a.rleExpand(0); err != nil {
		return false, err
	}
	if fb != nil && a.W[0] > 0 && a.G > 0 {
		a.paintTiles(fb, packetType)
	}
	return true, nil
}

// rleExpand implements u.java a(int) — expand S[idx][0..U[idx]] into
// X[idx], writing W[idx] valid bytes.
//
//	if A==0 && B==0: passthrough, W = U.
//	Otherwise scan S: byte == B → next byte fills 3 slots
//	                  byte == A → n = next+1, out = next-next; special-case
//	                              n==1 → single A; n==2 → single B
//	                  else       → passthrough single byte
func (a *AvocentV2) rleExpand(idx int) error {
	if a.S[idx] == nil {
		return fmt.Errorf("avo2: rle idx %d no buf", idx)
	}
	n4 := a.U[idx]
	src := a.S[idx] // full buffer; loop bounded by n4
	if a.X[idx] == nil || len(a.X[idx]) < n4*8 {
		grow := n4 * 8
		if grow < 1024 {
			grow = 1024
		}
		a.X[idx] = make([]byte, grow)
	}
	dst := a.X[idx]
	if a.A == 0 && a.B == 0 {
		for i := 0; i < n4; i++ {
			dst[i] = src[i]
		}
		a.W[idx] = n4
		return nil
	}
	if a.A == a.B {
		return fmt.Errorf("avo2: rle A==B==%d", a.A)
	}
	i, o := 0, 0
	Aval := byte(a.A)
	Bval := byte(a.B)
	for i < n4 {
		b := src[i]
		if b == Bval {
			if i+1 >= n4 {
				return fmt.Errorf("avo2: rle B-trunc at %d", i)
			}
			v := src[i+1]
			i += 2
			if o+3 > len(dst) {
				return fmt.Errorf("avo2: rle out overflow")
			}
			dst[o] = v
			dst[o+1] = v
			dst[o+2] = v
			o += 3
			continue
		}
		if b == Aval {
			// Java reads S[i+1] and S[i+2] unconditionally. If beyond buffer
			// it throws OOB and is silently caught upstream. We check the
			// underlying S buffer, not U, since S may be padded past U.
			if i+1 >= len(a.S[idx]) {
				return fmt.Errorf("avo2: rle A-trunc at %d", i)
			}
			n := int(src[i+1]) + 1
			var v byte
			if i+2 < len(a.S[idx]) {
				v = a.S[idx][i+2]
			}
			if n == 1 {
				if o+1 > len(dst) {
					return fmt.Errorf("avo2: rle out overflow")
				}
				dst[o] = Aval
				o++
				i += 2
				continue
			}
			if n == 2 {
				if o+1 > len(dst) {
					return fmt.Errorf("avo2: rle out overflow")
				}
				dst[o] = Bval
				o++
				i += 2
				continue
			}
			if o+n > len(dst) {
				grow := len(dst) * 2
				if grow < o+n {
					grow = o + n
				}
				nd := make([]byte, grow)
				copy(nd, dst[:o])
				dst = nd
				a.X[idx] = nd
			}
			for k := 0; k < n; k++ {
				dst[o+k] = v
			}
			o += n
			i += 3
			continue
		}
		if o+1 > len(dst) {
			return fmt.Errorf("avo2: rle out overflow at %d", o)
		}
		dst[o] = b
		o++
		i++
	}
	a.W[idx] = o
	return nil
}
