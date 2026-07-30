//go:build !windows && (!darwin || !cgo) && (!linux || !cgo)

package vcard

import "fmt"

// StubReader is a no-op reader for platforms with no PC/SC backend.
// Windows is excluded because its backend needs no cgo (winscard.dll is
// bound dynamically), so it is always available there.
type StubReader struct{}

// NewStubReader creates a stub reader that reports no card present.
func NewStubReader() (*StubReader, error) {
	return &StubReader{}, nil
}

func (r *StubReader) Transmit(apdu []byte) ([]byte, error) {
	return nil, fmt.Errorf("no card present")
}

func (r *StubReader) Present() bool {
	return false
}

func (r *StubReader) ATR() []byte {
	return nil
}

func (r *StubReader) Close() error {
	return nil
}

// Name returns the stub reader name.
func (r *StubReader) Name() string { return "stub (no PC/SC)" }

// Status reports that no PC/SC reader is available on this platform.
func Status() ReaderStatus {
	return ReaderStatus{Reason: "PC/SC smart-card support is only built on macOS"}
}

// OpenReader has no real reader to open off-darwin.
func OpenReader() (Reader, error) {
	return nil, fmt.Errorf("PC/SC smart-card support is only built on macOS")
}
