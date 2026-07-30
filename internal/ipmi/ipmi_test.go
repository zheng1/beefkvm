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

// TestSDRExponentOffset guards the R/B exponent byte offset in a Type-01 SDR.
// Regression: these were read from body byte 25 (the accuracy/tolerance field)
// instead of byte 24, so every sensor parsed as Bexp=7/Rexp=0 and voltages came
// out 1000x high (P12V read 11716 V instead of 11.72 V).
func TestSDRExponentOffset(t *testing.T) {
	b := make([]byte, 48)
	b[19] = 58   // M low byte (P12V on the reference BMC)
	b[24] = 0xD0 // R exp = -3 (high nibble), B exp = 0 (low nibble)

	s, ok := parseFullSDR(Sensor{}, b)
	if !ok {
		t.Fatal("parseFullSDR rejected a well-formed body")
	}
	if s.Rexp != -3 {
		t.Errorf("Rexp = %d, want -3 (exponents must come from body byte 24)", s.Rexp)
	}
	if s.Bexp != 0 {
		t.Errorf("Bexp = %d, want 0", s.Bexp)
	}
	if s.M != 58 {
		t.Errorf("M = %d, want 58", s.M)
	}
	// 58 * 202 * 10^-3 = 11.716 V, matching ipmitool's 11.72.
	if got := s.Convert(202); got < 11.6 || got > 11.8 {
		t.Errorf("Convert(202) = %.4f, want ~11.716 V", got)
	}
}
