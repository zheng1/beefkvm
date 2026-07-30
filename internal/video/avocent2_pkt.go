package video

import (
	"encoding/binary"
	"fmt"
)

// handleType1: frame init.
// Layout: [1]=flags z, [2]=i, [3]=j (h/w of tile grid),
//
//	[4..5]=l (width), [6..7]=k (height), [8..9]=m,
//	[10]=n, [11]=o=F (bits-per-pixel),
//	[12..13]=p, [14..15]=q. Body starts at 16.
func (a *AvocentV2) handleType1(p []byte, fb *Framebuffer) error {
	if len(p) < 16 {
		return fmt.Errorf("avo2: type-1 short: %d", len(p))
	}
	z := p[1]
	a.E = int(binary.BigEndian.Uint16(p[4:6]))
	a.D = int(binary.BigEndian.Uint16(p[6:8]))
	a.F = int(p[11])
	a.y = z&8 > 0 // s()
	// bytes-per-pixel for buffer allocation
	bpp := a.F / 8
	if a.F >= 24 {
		bpp = 4
	}
	if a.F == 16 {
		bpp = 3
	}
	if a.F == 4 {
		bpp = 1
	}
	need := a.E * a.D * bpp
	if a.V < need {
		a.V = need
		a.S[0] = make([]byte, a.V)
		a.S[1] = make([]byte, a.V)
		a.X[0] = make([]byte, a.V)
		a.X[1] = make([]byte, a.V)
	}
	a.sawInit = true
	if fb != nil && (fb.W != a.E || fb.H != a.D) && a.E > 0 && a.D > 0 {
		fb.Resize(a.E, a.D)
	}
	return nil
}

// handleType2: buffered upper-stream (full-frame RLE), S[0].
// [1]=z flags, [2]=A, [3]=B (RLE escapes),
// [4..5]=l width, [6..7]=k height, [8]=t, [9]=i, [10]=j, body from 12.
func (a *AvocentV2) handleType2(p []byte, fb *Framebuffer) (bool, error) {
	if len(p) < 12 {
		return false, fmt.Errorf("avo2: type-2 short: %d", len(p))
	}
	z := p[1]
	a.A = int(p[2])
	a.B = int(p[3])
	body := p[12:]
	if z&1 > 0 {
		a.U[0] = 0
		a.W[0] = 0
	}
	if a.S[0] == nil {
		a.S[0] = make([]byte, a.V)
		a.X[0] = make([]byte, a.V)
	}
	nBody := len(body)
	if nBody > 0 {
		if a.U[0]+nBody > a.V {
			return false, fmt.Errorf("avo2: type-2 overflow at %d+%d>%d", a.U[0], nBody, a.V)
		}
		copy(a.S[0][a.U[0]:], body)
		a.U[0] += nBody
	}
	if z&2 == 0 {
		return false, nil // more packets to come
	}
	if err := a.rleExpand(0); err != nil {
		return false, err
	}
	return false, nil
}

// handleType3: differential lower-stream, S[1]. Same header shape as type 2.
func (a *AvocentV2) handleType3(p []byte, fb *Framebuffer) (bool, error) {
	if len(p) < 12 {
		return false, fmt.Errorf("avo2: type-3 short: %d", len(p))
	}
	z := p[1]
	a.A = int(p[2])
	a.B = int(p[3])
	body := p[12:]
	if z&1 > 0 {
		a.U[1] = 0
		a.W[1] = 0
	}
	if a.S[1] == nil {
		a.S[1] = make([]byte, a.V)
		a.X[1] = make([]byte, a.V)
	}
	nBody := len(body)
	if nBody > 0 {
		if a.U[1]+nBody > a.V {
			return false, fmt.Errorf("avo2: type-3 overflow")
		}
		copy(a.S[1][a.U[1]:], body)
		a.U[1] += nBody
	}
	if z&2 == 0 {
		return false, nil
	}
	if err := a.rleExpand(1); err != nil {
		return false, err
	}
	// Renderer o() paints X[1] over X[0] using diff codes. Full impl deferred;
	// for now we just complete the frame boundary.
	if fb != nil && a.W[0] > 0 && a.E > 0 && a.D > 0 {
		a.paintFullFrame(fb)
	}
	return true, nil
}
