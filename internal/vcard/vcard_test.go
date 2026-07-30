package vcard

import "testing"

// TestBackendAlwaysPresent guards the build-constraint pairing between the
// darwin PC/SC backend (darwin && cgo) and the stub (!darwin || !cgo). If those
// two ever stop covering every combination, this package fails to build on the
// uncovered one — which is how "darwin without cgo" silently broke before, even
// though the README promises CGO_ENABLED=0 builds work.
func TestBackendAlwaysPresent(t *testing.T) {
	// Both symbols must exist on every supported platform/cgo combination.
	// Referencing them is the whole point; calling Status() is also safe since
	// every backend returns a value rather than panicking when no reader exists.
	st := Status()
	if !st.Available && st.Reason == "" {
		t.Error("Status() reported unavailable without saying why")
	}
	if _, err := OpenReader(); err == nil {
		// A reader may genuinely be attached on a dev machine; nothing to assert.
		t.Log("a PC/SC reader is present on this host")
	}
}
