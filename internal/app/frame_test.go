package app

import (
	"fmt"
	"strings"
	"testing"
)

// fixedSize is the test seam: a framePrinter printing to buf on a terminal
// reporting w×h.
func testPrinter(buf *strings.Builder, w, h int) *framePrinter {
	return &framePrinter{
		out:  buf,
		size: func() (int, int, error) { return w, h, nil },
	}
}

func frameLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	return lines
}

// The duplicated-board bug: a frame taller than the terminal cannot be moved
// back over (the cursor stops at the top row), so every redraw would scroll
// its overflow into scrollback. The printer must never emit more rows than
// the screen keeps — height-1, leaving the cursor its resting row — and must
// move up exactly as many rows as it last printed.
func TestFrameTallerThanTerminalIsCutToFit(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf, 80, 8)

	fp.print(frameLines(13))
	first := buf.String()
	if got := strings.Count(first, "\x1b[2K"); got != 7 {
		t.Fatalf("first frame printed %d rows on an 8-row terminal, want 7", got)
	}
	if strings.Contains(first, "line 7") {
		t.Fatalf("first frame kept rows past the screen: %q", first)
	}

	buf.Reset()
	fp.print(frameLines(13))
	second := buf.String()
	if !strings.HasPrefix(second, "\x1b[7A") {
		t.Fatalf("second frame must move up over the 7 rows actually printed, got %q", second)
	}
	if got := strings.Count(second, "\x1b[2K"); got != 7 {
		t.Fatalf("second frame printed %d rows, want 7", got)
	}
}

// A narrowing terminal may rewrap the previous frame's rows (reflow), and a
// height now below the frame leaves no room to move back over it — both
// falsify prev, so the printer must repaint from a cleared screen rather
// than move up a broken count.
func TestResizeThatFalsifiesPrevRepaintsFromClearedScreen(t *testing.T) {
	cases := []struct {
		name string
		w, h int
	}{
		{"narrower", 60, 24},
		{"shorter than frame", 80, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			w, h := 80, 24
			fp := &framePrinter{
				out:  &buf,
				size: func() (int, int, error) { return w, h, nil },
			}
			fp.print(frameLines(10))

			buf.Reset()
			w, h = tc.w, tc.h
			fp.print(frameLines(10))
			out := buf.String()
			if !strings.HasPrefix(out, "\x1b[H\x1b[J") {
				t.Fatalf("resize must home and clear before repainting, got %q", out)
			}
			if strings.Contains(out, "\x1b[10A") {
				t.Fatalf("resize must not trust the stale row count, got %q", out)
			}
		})
	}
}

// Growing wider never rewraps a width-clipped line, so it keeps the cheap
// in-place path: no clear, plain move-up.
func TestWideningKeepsTheInPlaceRedraw(t *testing.T) {
	var buf strings.Builder
	w := 80
	fp := &framePrinter{
		out:  &buf,
		size: func() (int, int, error) { return w, 24, nil },
	}
	fp.print(frameLines(10))

	buf.Reset()
	w = 120
	fp.print(frameLines(10))
	out := buf.String()
	if strings.Contains(out, "\x1b[H") {
		t.Fatalf("widening must not force a clear-screen repaint, got %q", out)
	}
	if !strings.HasPrefix(out, "\x1b[10A") {
		t.Fatalf("widening keeps the move-up redraw, got %q", out)
	}
}

// Some ptys report 0×0 without an error. An unknown size must neither cut
// the frame nor masquerade as a resize that forces a clear-repaint on every
// draw — it keeps the plain uncut in-place path.
func TestZeroSizeReportKeepsThePlainPath(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf, 0, 0)
	fp.print(frameLines(10))

	buf.Reset()
	fp.print(frameLines(10))
	out := buf.String()
	if strings.Contains(out, "\x1b[H") {
		t.Fatalf("unknown size must not force a clear-screen repaint, got %q", out)
	}
	if !strings.HasPrefix(out, "\x1b[10A") {
		t.Fatalf("unknown size keeps the full move-up redraw, got %q", out)
	}
	if got := strings.Count(out, "\x1b[2K"); got != 10 {
		t.Fatalf("unknown size must not cut the frame: printed %d rows, want 10", got)
	}
}

// fitTop is the board's viewport: the selected block scrolls into view whole,
// its first line winning when the block outgrows the viewport.
func TestFitTop(t *testing.T) {
	cases := []struct {
		name                               string
		top, selStart, selEnd, total, view int
		want                               int
	}{
		{"everything above stays put", 0, 0, 4, 20, 10, 0},
		{"selection below scrolls down", 0, 12, 16, 20, 10, 6},
		{"selection above scrolls up", 8, 4, 8, 20, 10, 4},
		{"tail never over-scrolls", 15, 16, 20, 20, 10, 10},
		{"oversized block shows its first line", 0, 4, 20, 30, 10, 4},
		{"never negative", 5, 0, 2, 6, 10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fitTop(tc.top, tc.selStart, tc.selEnd, tc.total, tc.view); got != tc.want {
				t.Fatalf("fitTop(%d,%d,%d,%d,%d) = %d, want %d",
					tc.top, tc.selStart, tc.selEnd, tc.total, tc.view, got, tc.want)
			}
		})
	}
}
