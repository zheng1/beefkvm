package video

import (
	"testing"
)

// TestRLEPassthrough: A==0 && B==0 → identity copy.
func TestRLEPassthrough(t *testing.T) {
	a := NewAvocentV2()
	a.V = 32
	a.S[0] = make([]byte, 32)
	a.X[0] = make([]byte, 32)
	src := []byte{1, 2, 3, 4, 5, 6}
	copy(a.S[0], src)
	a.U[0] = len(src)
	a.A = 0
	a.B = 0
	if err := a.rleExpand(0); err != nil {
		t.Fatalf("rle: %v", err)
	}
	if a.W[0] != len(src) {
		t.Fatalf("W=%d want %d", a.W[0], len(src))
	}
	for i, v := range src {
		if a.X[0][i] != v {
			t.Fatalf("mismatch @%d: got %d want %d", i, a.X[0][i], v)
		}
	}
}

// TestRLEBEscape: byte==B → next byte fills 3 slots.
func TestRLEBEscape(t *testing.T) {
	a := NewAvocentV2()
	a.V = 32
	a.S[0] = make([]byte, 32)
	a.X[0] = make([]byte, 32)
	// A=0xFE, B=0xFF. Input: [0x11, 0xFF, 0x77, 0x22] → out [0x11, 0x77, 0x77, 0x77, 0x22]
	a.A = 0xFE
	a.B = 0xFF
	src := []byte{0x11, 0xFF, 0x77, 0x22}
	copy(a.S[0], src)
	a.U[0] = len(src)
	if err := a.rleExpand(0); err != nil {
		t.Fatalf("rle: %v", err)
	}
	want := []byte{0x11, 0x77, 0x77, 0x77, 0x22}
	if a.W[0] != len(want) {
		t.Fatalf("W=%d want %d out=%v", a.W[0], len(want), a.X[0][:a.W[0]])
	}
	for i, v := range want {
		if a.X[0][i] != v {
			t.Fatalf("mismatch @%d: got %d want %d", i, a.X[0][i], v)
		}
	}
}

// TestRLEARun: byte==A → n+1 copies of value.
func TestRLEARun(t *testing.T) {
	a := NewAvocentV2()
	a.V = 32
	a.S[0] = make([]byte, 32)
	a.X[0] = make([]byte, 32)
	a.A = 0xFE
	a.B = 0xFF
	// [0x11, 0xFE, 0x04, 0x88, 0x22] → 0x11, then 5 copies of 0x88, then 0x22
	src := []byte{0x11, 0xFE, 0x04, 0x88, 0x22}
	copy(a.S[0], src)
	a.U[0] = len(src)
	if err := a.rleExpand(0); err != nil {
		t.Fatalf("rle: %v", err)
	}
	want := []byte{0x11, 0x88, 0x88, 0x88, 0x88, 0x88, 0x22}
	if a.W[0] != len(want) {
		t.Fatalf("W=%d want %d out=%v", a.W[0], len(want), a.X[0][:a.W[0]])
	}
	for i, v := range want {
		if a.X[0][i] != v {
			t.Fatalf("mismatch @%d: got %d want %d", i, a.X[0][i], v)
		}
	}
}

// TestRLEALiteralA: n=1 → literal A. Input [0xFE, 0x00] (U=2, i+=2) → [0xFE].
func TestRLEALiteralA(t *testing.T) {
	a := NewAvocentV2()
	a.V = 32
	a.S[0] = make([]byte, 32)
	a.X[0] = make([]byte, 32)
	a.A = 0xFE
	a.B = 0xFF
	src := []byte{0xFE, 0x00, 0x00}
	copy(a.S[0], src)
	a.U[0] = 2
	if err := a.rleExpand(0); err != nil {
		t.Fatalf("rle: %v", err)
	}
	if a.W[0] != 1 || a.X[0][0] != 0xFE {
		t.Fatalf("W=%d out[0]=%d", a.W[0], a.X[0][0])
	}
}

// TestRLELiteralB: n=2 → literal B. i+2 must be readable; i+=2 only consumes 2.
func TestRLELiteralB(t *testing.T) {
	a := NewAvocentV2()
	a.V = 32
	a.S[0] = make([]byte, 32)
	a.X[0] = make([]byte, 32)
	a.A = 0xFE
	a.B = 0xFF
	src := []byte{0xFE, 0x01, 0x00}
	copy(a.S[0], src)
	a.U[0] = 2
	if err := a.rleExpand(0); err != nil {
		t.Fatalf("rle: %v", err)
	}
	if a.W[0] != 1 || a.X[0][0] != 0xFF {
		t.Fatalf("W=%d out[0]=%d", a.W[0], a.X[0][0])
	}
}
