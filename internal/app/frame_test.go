package app

import (
	"fmt"
	"strings"
	"testing"
)

func testPrinter(buf *strings.Builder) *framePrinter {
	return &framePrinter{out: buf}
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
// the screen holds, and must move up exactly the rows it last printed.
func TestFrameTallerThanTerminalIsCutToFit(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf)

	fp.print(frameLines(13), 80, 8)
	first := buf.String()
	if got := strings.Count(first, "\r\n"); got != 7 {
		t.Fatalf("first frame advanced %d rows on an 8-row terminal, want 7", got)
	}
	if strings.Contains(first, "line 8") {
		t.Fatalf("first frame kept rows past the screen: %q", first)
	}

	buf.Reset()
	fp.print(frameLines(13), 80, 8)
	second := buf.String()
	if !strings.HasPrefix(second, "\r\x1b[7A") {
		t.Fatalf("second frame must move up over the 8 rows actually printed, got %q", second)
	}
	if got := strings.Count(second, "\r\n"); got != 7 {
		t.Fatalf("second frame advanced %d rows, want 7", got)
	}
}

// A one-row terminal is the degenerate frame: one line, rewritten in place.
// Scrolling is impossible only if the printer never emits a newline at all.
func TestOneRowTerminalNeverScrolls(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf)
	for range 3 {
		fp.print(frameLines(5), 80, 1)
	}
	out := buf.String()
	if strings.Contains(out, "\n") {
		t.Fatalf("a one-row terminal must never see a newline, got %q", out)
	}
	if !strings.Contains(out, "line 0") || strings.Contains(out, "line 1") {
		t.Fatalf("a one-row terminal shows exactly the first line, got %q", out)
	}
}

// A shrinking frame must clear the rows the taller one still occupies, and
// the next move-up must count the new frame, not the old.
func TestShrinkingFrameClearsItsLeftovers(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf)
	fp.print(frameLines(10), 80, 24)

	buf.Reset()
	fp.print(frameLines(4), 80, 24)
	if out := buf.String(); !strings.Contains(out, "\x1b[J") {
		t.Fatalf("shrunk frame must clear below its last line, got %q", out)
	}

	buf.Reset()
	fp.print(frameLines(4), 80, 24)
	if out := buf.String(); !strings.HasPrefix(out, "\r\x1b[3A") {
		t.Fatalf("move-up must count the shrunk frame, got %q", out)
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
			fp := testPrinter(&buf)
			fp.print(frameLines(10), 80, 24)

			buf.Reset()
			fp.print(frameLines(10), tc.w, tc.h)
			out := buf.String()
			if !strings.HasPrefix(out, "\x1b[H\x1b[J") {
				t.Fatalf("resize must home and clear before repainting, got %q", out)
			}
			if strings.Contains(out, "\x1b[9A") {
				t.Fatalf("resize must not trust the stale row count, got %q", out)
			}
		})
	}
}

// Growing wider never rewraps a width-clipped line, so it keeps the cheap
// in-place path: no clear-screen, plain move-up.
func TestWideningKeepsTheInPlaceRedraw(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf)
	fp.print(frameLines(10), 80, 24)

	buf.Reset()
	fp.print(frameLines(10), 120, 24)
	out := buf.String()
	if strings.Contains(out, "\x1b[H") {
		t.Fatalf("widening must not force a clear-screen repaint, got %q", out)
	}
	if !strings.HasPrefix(out, "\r\x1b[9A") {
		t.Fatalf("widening keeps the move-up redraw, got %q", out)
	}
}

// The picker refuses to open on a tty that reports no size; geometry's
// assumption exists for a size that goes unreadable mid-session, where it
// bounds the damage instead of trusting an unknown screen.
func TestGeometryAssumesConservativelyWhenTheSizeVanishes(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		err  error
	}{
		{"zero size", 0, 0, nil},
		{"error", 0, 0, fmt.Errorf("ioctl failed")},
		{"negative", -1, 24, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &framePrinter{size: func() (int, int, error) { return tc.w, tc.h, tc.err }}
			if w, h := fp.geometry(); w != assumeWidth || h != assumeHeight {
				t.Fatalf("geometry() = %d×%d, want the assumed %d×%d", w, h, assumeWidth, assumeHeight)
			}
		})
	}
}

// finish steps below the frame exactly once, so cooked-mode output lands on
// its own row while the last frame stays on screen.
func TestFinishStepsBelowTheFrameOnce(t *testing.T) {
	var buf strings.Builder
	fp := testPrinter(&buf)
	fp.print(frameLines(3), 80, 24)
	buf.Reset()
	fp.finish()
	fp.finish()
	if got := buf.String(); got != "\r\n" {
		t.Fatalf("finish writes one row step, got %q", got)
	}
}

// boardWindow ranks what a chooser needs: the selected block stays visible at
// every height, the footer stands beside it whenever a body row fits, and on
// a terminal too short for both the selection wins.
func TestBoardWindowKeepsTheSelectionAtEveryHeight(t *testing.T) {
	// A 10-line body whose selected block spans lines 4–7, under a 2-line
	// footer — the shape of a multi-account board.
	const selStart, selEnd, bodyLen, footerLen = 4, 8, 10, 2
	for h := 1; h <= 14; h++ {
		top, view, keepFooter := boardWindow(0, selStart, selEnd, bodyLen, footerLen, h)
		if view < 1 || top < 0 || top+view > bodyLen {
			t.Fatalf("h=%d: window [%d,%d) out of a %d-line body", h, top, top+view, bodyLen)
		}
		if selStart < top || selStart >= top+view {
			t.Fatalf("h=%d: selected block's first line %d outside window [%d,%d)", h, selStart, top, top+view)
		}
		rows := view
		if keepFooter {
			rows += footerLen
		}
		if rows > h && h < bodyLen+footerLen {
			t.Fatalf("h=%d: window emits %d rows", h, rows)
		}
		if h >= 3 && !keepFooter {
			t.Fatalf("h=%d: footer dropped though a body row could stand beside it", h)
		}
		if h < 3 && keepFooter {
			t.Fatalf("h=%d: footer kept at the selection's expense", h)
		}
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
