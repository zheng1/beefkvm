package video

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTraceFirstAggregation runs the aggregated first frame through the
// DCT decoder with dctBlockDebug enabled, so we can see how many bits
// each DCT block consumes. Java DCT blocks typically consume 80-200 bits
// each; if my blocks are shorter, EOB is triggering wrongly.
func TestTraceFirstAggregation(t *testing.T) {
	dir := "../../testdata/tiles"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no captured tiles")
	}
	dec := NewAvocentDecoder()
	fb := NewFramebuffer(1024, 768)

	// Aggregate tiles 1-13 (first big frame)
	var aggHeader []byte
	var aggBody []byte
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "tile-") || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		var idx, id, ln int
		fmt.Sscanf(e.Name(), "tile-%04d-id%d-len%d.bin", &idx, &id, &ln)
		if id != 134 || idx == 0 {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if len(data) < 12 {
			continue
		}
		startsNew := (data[8] & 1) != 0
		if startsNew && aggHeader != nil {
			break // stop at 2nd aggregation
		}
		if startsNew {
			aggHeader = append([]byte(nil), data[:12]...)
			aggBody = append([]byte(nil), data[12:]...)
		} else {
			aggBody = append(aggBody, data[12:]...)
		}
	}

	if aggHeader == nil {
		t.Skip("no aggregation captured")
	}

	dctBlockDebug = func(f string, a ...any) {
		t.Logf("  DCT "+f, a...)
	}
	defer func() { dctBlockDebug = nil }()

	mode := aggHeader[0] & 1
	luma := aggHeader[11] & 0x0F
	chroma := (aggHeader[11] >> 4) & 0x0F
	dec.SetMode(mode, luma, chroma)
	dec.Decode(fb, 0, 0, aggBody)
	t.Logf("done: ops=%d trail=%v", dec.OpCount(), dec.LastOpTrail())
}
