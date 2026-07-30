package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPaletteOnlyTiles: check how many opcodes each tile can process
// if we STUB DCT (just consume 4 bits, no decode). If all tiles run to
// END (opcode 9), palette-cache logic is bit-perfect and DCT is the
// only desync source.
func TestPaletteOnlyTiles(t *testing.T) {
	dir := "../../testdata/tiles"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no captured tiles")
	}
	dctStubMode = true
	defer func() { dctStubMode = false }()
	dec := NewAvocentDecoder()
	fb := NewFramebuffer(1024, 768)
	reachedEnd := 0
	tilesChecked := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "tile-") || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		var idx, id, ln int
		if _, err := stubSscanf(e.Name(), &idx, &id, &ln); err != nil {
			continue
		}
		if id != 134 && id != 34310 {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if len(data) < 12 {
			continue
		}
		mode := data[0] & 1
		lumaQ := data[11] & 0x0F
		chromaQ := (data[11] >> 4) & 0x0F
		dec.SetMode(mode, lumaQ, chromaQ)
		dec.Decode(fb, 0, 0, data[12:])
		trail := dec.LastOpTrail()
		// Did it reach END (op 9)?
		endedClean := len(trail) > 0 && trail[len(trail)-1] == 9
		if endedClean {
			reachedEnd++
		}
		tilesChecked++
		if !endedClean {
			t.Logf("tile %s did NOT reach END; last-ops=%v", e.Name(), trail)
		}
	}
	t.Logf("PALETTE-ONLY: %d/%d tiles ended cleanly with END opcode", reachedEnd, tilesChecked)
}

// dctStubMode is defined in avocent.go

func stubSscanf(name string, idx, id, ln *int) (int, error) {
	// Very small sscanf: tile-XXXX-idYYY-lenZZZZ.bin
	var v [3]int
	pos := 5 // skip "tile-"
	fields := 0
	for pos < len(name) && fields < 3 {
		start := pos
		if fields > 0 {
			// skip prefix
			for pos < len(name) && (name[pos] < '0' || name[pos] > '9') {
				pos++
			}
			start = pos
		}
		for pos < len(name) && name[pos] >= '0' && name[pos] <= '9' {
			v[fields] = v[fields]*10 + int(name[pos]-'0')
			pos++
		}
		if pos == start {
			break
		}
		fields++
	}
	*idx = v[0]
	*id = v[1]
	*ln = v[2]
	return fields, nil
}
