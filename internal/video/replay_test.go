package video

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplayCapturedTiles decodes each captured tile file and reports
// how many pixels ended up non-black. Purely offline: lets us tune the
// decoder without BMC iteration.
//
// Multi-fragment aggregation: tile files with byte[8] & 1 == 1 start a
// new logical packet; subsequent files with byte[8] & 1 == 0 append
// their payload (bytes[12:]) to the aggregation. The decoder runs once
// per aggregated packet.
func TestReplayCapturedTiles(t *testing.T) {
	dir := "../../testdata/tiles"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no captured tiles")
	}
	dec := NewAvocentDecoder()
	fb := NewFramebuffer(1024, 768)
	tileCount := 0
	nonBlackTotal := 0

	// Aggregation state
	var aggHeader []byte
	var aggBody []byte
	var aggName string

	flushAggregation := func() {
		if aggHeader == nil {
			return
		}
		// Reassemble full packet: header + body. Header parsing is now
		// in DecodePacket — matches production main.go path exactly.
		pkt := make([]byte, 0, 12+len(aggBody))
		pkt = append(pkt, aggHeader...)
		pkt = append(pkt, aggBody...)
		before := countNonBlack(fb)
		dec.DecodePacket(fb, pkt)
		after := countNonBlack(fb)
		delta := after - before
		nonBlackTotal += delta
		trail := dec.LastOpTrail()
		bitsConsumed := (dec.PosBytes() * 8) - dec.BufBits()
		t.Logf("agg %s bytes=%d bits_consumed=%d ops=%d trail=%v → +%d px",
			aggName, len(aggBody), bitsConsumed, dec.OpCount(), trail, delta)
		tileCount++
		aggHeader = nil
		aggBody = nil
		aggName = ""
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "tile-") || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		var idx, id, ln int
		fmt.Sscanf(e.Name(), "tile-%04d-id%d-len%d.bin", &idx, &id, &ln)
		if id != 134 && id != 34310 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 12 {
			continue
		}
		// Java m.a(ib): byte[8] & 1 = k = "start new aggregation" flag.
		// k=1 → flush any prior aggregation, start new with this header.
		// k=0 → append this body (data[12:]) to the current aggregation.
		startsNew := (data[8] & 1) != 0
		if startsNew {
			flushAggregation()
			aggHeader = append([]byte(nil), data[:12]...)
			aggBody = append([]byte(nil), data[12:]...)
			aggName = e.Name()
		} else {
			if aggHeader == nil {
				// Orphan continuation with no prior start — decode standalone.
				aggHeader = append([]byte(nil), data[:12]...)
				aggBody = append([]byte(nil), data[12:]...)
				aggName = e.Name() + " (orphan)"
				flushAggregation()
				continue
			}
			aggBody = append(aggBody, data[12:]...)
			aggName = aggName + "+" + e.Name()
		}
	}
	flushAggregation()

	t.Logf("total: %d aggregated packets, %d non-black pixels", tileCount, nonBlackTotal)
	// Also dump the framebuffer as raw RGB for inspection.
	dumpFB("../../testdata/tiles-out.rgb", fb)
}

func countNonBlack(fb *Framebuffer) int {
	n := 0
	for _, p := range fb.Pixels {
		if p&0xFFFFFF != 0 {
			n++
		}
	}
	return n
}

func dumpFB(path string, fb *Framebuffer) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 3*fb.W*fb.H)
	for i, p := range fb.Pixels {
		buf[i*3+0] = byte(p >> 16)
		buf[i*3+1] = byte(p >> 8)
		buf[i*3+2] = byte(p)
	}
	f.Write(buf)
}
