package video

import (
	"testing"
)

// TestDumpHuffman dumps the constructed Huffman min/max/vals for
// verification against d.java expected values.
func TestDumpHuffman(t *testing.T) {
	tables := []struct {
		name string
		h    *avoHuff
	}{
		{"DC-luma", avoDcLuma},
		{"DC-chroma", avoDcChroma},
		{"AC-luma", avoAcLuma},
		{"AC-chroma", avoAcChroma},
	}
	for _, tab := range tables {
		t.Logf("=== %s ===", tab.name)
		for l := 1; l <= 16; l++ {
			nv := int(tab.h.maxCode[l] - tab.h.minCode[l] + 1)
			if tab.h.maxCode[l] < 0 {
				nv = 0
			}
			t.Logf("  l=%2d min=%5d max=%5d n=%d", l, tab.h.minCode[l], tab.h.maxCode[l], nv)
		}
	}
}
