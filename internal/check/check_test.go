package check

import (
	"strings"
	"testing"
	"testing/iotest"
)

// The overlap carry is the subtle part: a needle split across a chunk
// boundary must still be found, for every split position and even when the
// needle is longer than the read buffer.
func TestSearchReader(t *testing.T) {
	needles := []string{"NEEDLE", "api/oauth"}
	cases := []struct {
		name    string
		content string
		bufSize int
		want    map[string]bool
	}{
		{"inside one chunk", "xxNEEDLExx api/oauth xx", 64,
			map[string]bool{"NEEDLE": true, "api/oauth": true}},
		{"split across boundary", "xxxxxxNEEDLExxxx", 8, // boundary at E|DLE
			map[string]bool{"NEEDLE": true}},
		{"needle longer than buffer", "xxNEEDLExx", 3,
			map[string]bool{"NEEDLE": true}},
		{"missing", "nothing here", 8, map[string]bool{}},
		{"at the very end", "xxxxxxxxxxNEEDLE", 8, map[string]bool{"NEEDLE": true}},
	}
	for _, c := range cases {
		got := searchReader(strings.NewReader(c.content), needles, c.bufSize)
		for n, want := range c.want {
			if got[n] != want {
				t.Errorf("%s: found[%q] = %v, want %v", c.name, n, got[n], want)
			}
		}
		if len(c.want) == 0 && len(got) != 0 {
			t.Errorf("%s: unexpected finds %v", c.name, got)
		}
	}

	// Exhaustive: split "NEEDLE" at every boundary a 4-byte buffer produces
	// for every prefix length.
	for pad := 0; pad < 12; pad++ {
		content := strings.Repeat("x", pad) + "NEEDLE" + strings.Repeat("y", 3)
		got := searchReader(strings.NewReader(content), []string{"NEEDLE"}, 4)
		if !got["NEEDLE"] {
			t.Errorf("pad %d: needle lost at chunk boundary", pad)
		}
	}

	// A reader that returns data and EOF in the same call (io.Reader allows
	// it) must not drop the final chunk.
	r := iotest.DataErrReader(strings.NewReader("xxxxxxNEEDLE"))
	if got := searchReader(r, []string{"NEEDLE"}, 8); !got["NEEDLE"] {
		t.Error("data+EOF read dropped the final chunk")
	}
}
