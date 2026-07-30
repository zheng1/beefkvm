package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBitOrderCompare runs the same tiles in BE and LE mode, reports
// which produces more tiles reaching END.
func TestBitOrderCompare(t *testing.T) {
	dir := "../../testdata/tiles"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no captured tiles")
	}
	dctStubMode = true
	defer func() { dctStubMode = false }()
	files := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bin") && strings.HasPrefix(e.Name(), "tile-") {
			var idx, id, ln int
			stubSscanf(e.Name(), &idx, &id, &ln)
			if id == 134 || id == 34310 {
				files = append(files, e.Name())
			}
		}
	}
	for _, order := range []bool{false, true} {
		BitOrderLE = order
		dec := NewAvocentDecoder()
		fb := NewFramebuffer(1024, 768)
		end := 0
		endedTiles := []string{}
		for _, name := range files {
			data, _ := os.ReadFile(filepath.Join(dir, name))
			if len(data) < 12 {
				continue
			}
			mode := data[0] & 1
			lumaQ := data[11] & 0x0F
			chromaQ := (data[11] >> 4) & 0x0F
			dec.SetMode(mode, lumaQ, chromaQ)
			dec.Decode(fb, 0, 0, data[12:])
			trail := dec.LastOpTrail()
			if len(trail) > 0 && trail[len(trail)-1] == 9 {
				end++
				endedTiles = append(endedTiles, name)
			}
		}
		label := "BE"
		if order {
			label = "LE"
		}
		t.Logf("%s: %d/%d tiles reached END: %v", label, end, len(files), endedTiles)
	}
	BitOrderLE = false
}
