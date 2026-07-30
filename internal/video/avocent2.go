// Package video — Avocent packet-134 codec, rewrite matching u.java.
//
// This file replaces the DCT/JPEG code in avocent.go / avocent_dct.go /
// jpeg.go — those files implement a codec that does not exist on the
// wire. The real packet-134 codec is a stateful packet-type dispatcher
// keyed on payload byte 0:
//
//	0  dimension probe (E/D width/height)
//	1  frame init (dims + bits-per-pixel F + block-grid + escapes)
//	2  full-frame RLE stream, S[0] buffer (paired with type 3)
//	3  differential RLE stream, S[1] buffer (paired with type 2)
//	4  tile-list frame + RLE, renderer l() — full-frame RGB refresh
//	5  tile-list frame + RLE, renderer k() — differential RGB
//	6  tile-list frame + RLE, renderer m() — 24-bit direct
//	7  init variant
//	8  init variant with cursor position
//
// See decompiled/kvm/com/avocent/kvm/b/u.java (setData + main switch).
// See decompiled/kvm/com/avocent/kvm/b/a/vb.java for the byte layout of
// each packet type.
package video

import (
	"encoding/binary"
	"fmt"
)

// AvocentTileDesc is one entry of the tile list from packet types 4/5/6.
// Fields match com.avocent.kvm.b.o exactly (a/b/c/d/e/f/g in the Java
// source; a is x2, b is x1, c is y2, d is y1).
type AvocentTileDesc struct {
	X2, X1, Y2, Y1 int
	W, H, N        int
}

// AvocentV2 is a stateful decoder for the packet-134 codec. All state
// mirrors u.java field naming (letters preserved for cross-reference).
type AvocentV2 struct {
	// screen dims (E=width, D=height, F=bits-per-pixel)
	E, D, F int
	// RLE escapes A, B (u.java fields, from packet header bytes 2/3)
	A, B int
	// per-frame tile count and descriptor pool
	G int
	J []AvocentTileDesc
	// S[0/1] input byte buffers, X[0/1] decompressed byte planes
	S [2][]byte
	X [2][]byte
	// U[0/1] = write positions into S[]; W[0/1] = valid length of X[]
	U [2]int
	W [2]int
	// V = size of S[0]/S[1]/X[0]/X[1] allocations (matches u.java field V)
	V int
	// y flag: uses 5-6-5 packing when true (from vb.s())
	y bool
	// hb/ib/jb: flags parsed from packet-3 header byte 1
	hb, ib, jb bool
	// last width/height as reported by dim-probe (packet type 0)
	probeW, probeH int
	// last observed dimensions log
	sawInit bool
}

// NewAvocentV2 returns a fresh decoder.
func NewAvocentV2() *AvocentV2 {
	return &AvocentV2{
		V: 131072,
		J: make([]AvocentTileDesc, 64),
	}
}

// Decode consumes one full packet-134 payload and writes pixels into fb.
// Returns whether the frame is complete (r-flag set) and any error.
func (a *AvocentV2) Decode(fb *Framebuffer, payload []byte) (bool, error) {
	if len(payload) < 1 {
		return false, fmt.Errorf("avo2: empty payload")
	}
	h := int(payload[0])
	switch h {
	case 0:
		return false, a.handleType0(payload)
	case 1:
		return false, a.handleType1(payload, fb)
	case 2:
		return a.handleType2(payload, fb)
	case 3:
		return a.handleType3(payload, fb)
	case 4:
		return a.handleType4(payload, fb)
	case 5:
		return a.handleType4(payload, fb) // renderer variant handled inside
	case 6:
		return a.handleType4(payload, fb)
	case 7, 8:
		// init variants, safely ignored for now
		return false, nil
	default:
		return false, fmt.Errorf("avo2: unknown packet type %d", h)
	}
}

// handleType0: dimension probe. Layout: [1]=x (byte), [2..3]=l (short, w),
// [4..5]=k (short, h). We just note the reported size.
func (a *AvocentV2) handleType0(p []byte) error {
	if len(p) < 6 {
		return fmt.Errorf("avo2: type-0 short: %d", len(p))
	}
	a.probeW = int(binary.BigEndian.Uint16(p[2:4]))
	a.probeH = int(binary.BigEndian.Uint16(p[4:6]))
	return nil
}
