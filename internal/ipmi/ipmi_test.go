package ipmi

import "testing"

// TestSignExtend sanity-checks the signed-arithmetic helpers used to
// parse M/B and their exponents from an SDR record.
func TestSignExtend(t *testing.T) {
	// 10-bit: 0x3FF should decode as -1
	if got := signExtend10(0x03FF); got != -1 {
		t.Errorf("signExtend10(0x3FF) = %d, want -1", got)
	}
	// 10-bit: 0x001 should decode as +1
	if got := signExtend10(0x0001); got != 1 {
		t.Errorf("signExtend10(0x001) = %d, want 1", got)
	}
	// 12-bit: 0xFFF should decode as -1
	if got := signExtend12(0x0FFF); got != -1 {
		t.Errorf("signExtend12(0xFFF) = %d, want -1", got)
	}
	// 4-bit: 0xF should decode as -1
	if got := sign4(0x0F); got != -1 {
		t.Errorf("sign4(0xF) = %d, want -1", got)
	}
	// 4-bit: 0x7 should decode as +7
	if got := sign4(0x07); got != 7 {
		t.Errorf("sign4(0x7) = %d, want 7", got)
	}
}

// TestConvertLinear ensures the classic "M=1, B=0, R=-1" temperature-in-
// tenths-of-a-degree formula turns a raw byte back into a Celsius float.
func TestConvertLinear(t *testing.T) {
	s := Sensor{
		Analog: 2, // 2's complement
		Linear: 0, // linear
		M:      1,
		B:      0,
		Bexp:   0,
		Rexp:   -1,
	}
	// raw = 100 (2's compl) → (1 * 100 + 0) * 10^-1 = 10.0
	if got := s.Convert(100); got != 10 {
		t.Errorf("Convert(100) = %v, want 10", got)
	}
}
