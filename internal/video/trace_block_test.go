package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTraceFirstTileBlocks decodes tile-0002 with per-block trace.
// Expected: 6 blocks per MCU (mode=1 4:2:2 = 4 Y + 1 Cb + 1 Cr).
func TestTraceFirstTileBlocks(t *testing.T) {
	dir := "../../testdata/tiles"
	entries, _ := os.ReadDir(dir)
	var target string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tile-0002-") {
			target = e.Name()
			break
		}
	}
	if target == "" {
		t.Skip("no tile 0002")
	}
	data, _ := os.ReadFile(filepath.Join(dir, target))
	dctBlockDebug = func(f string, a ...any) { t.Logf(f, a...) }
	defer func() { dctBlockDebug = nil }()
	dec := NewAvocentDecoder()
	fb := NewFramebuffer(1024, 768)
	mode := data[0] & 1
	lumaQ := data[11] & 0x0F
	chromaQ := (data[11] >> 4) & 0x0F
	dec.SetMode(mode, lumaQ, chromaQ)
	dec.Decode(fb, 0, 0, data[12:])
	t.Logf("ops=%d trail=%v", dec.OpCount(), dec.LastOpTrail())
}
