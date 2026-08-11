package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/render"
)

// Geometry assumed when the terminal reports no usable size — a rare tty
// answers 0×0 without an error. The historical 80×24: a wrong guess costs
// cosmetic truncation, while trusting an unknown height costs unbounded
// scrollback duplication.
const (
	assumeWidth  = 80
	assumeHeight = 24
)

// framePrinter redraws a frame in place: move up over the previous frame and
// rewrite every line (\x1b[2K clears remnants of longer ones; \x1b[J before
// the last line clears rows a taller previous frame still occupies). The
// move-up arithmetic holds only while every printed line is one physical row
// still standing where it was printed, so three guards protect it. Lines are
// clipped to the terminal width — a wrapped line would occupy two rows. A
// frame is cut to the terminal height — the cursor cannot move above the
// screen, so a taller frame would scroll on every redraw and push a copy of
// its top into scrollback each time (the duplicated-board bug); the last line
// carries no trailing newline, which is what lets a frame of exactly the
// screen's height (a one-row terminal included) redraw without ever
// scrolling. And a resize that could have rewrapped the previous frame's
// rows — the terminal narrowed, or shrank below the frame — falsifies prev,
// so the printer repaints from a cleared screen instead of moving up a count
// the resize just broke.
type framePrinter struct {
	prev int
	w    int // width the previous frame was printed at

	// Test seams; nil means the real stdout terminal.
	out  io.Writer
	size func() (w, h int, err error)
}

// geometry is the one reading a frame is built and printed from — the layout
// windows to it and print receives the same snapshot, so a resize landing
// between the two cannot hand the printer a frame fitted to a screen that no
// longer exists. The picker refuses to open on a tty that reports no size;
// the assumed geometry is the safety net for a size that goes unreadable
// mid-session, bounding the damage instead of trusting an unknown screen.
func (f *framePrinter) geometry() (w, h int) {
	getSize := f.size
	if getSize == nil {
		getSize = func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }
	}
	w, h, err := getSize()
	if err != nil || w <= 0 || h <= 0 {
		return assumeWidth, assumeHeight
	}
	return w, h
}

func (f *framePrinter) print(lines []string, width, height int) {
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	var b strings.Builder
	// prev counts physical rows, and only these two resizes can change how
	// many rows the old frame occupies: narrowing rewraps them in a
	// reflowing terminal, and a height below the frame leaves no room to
	// move back over it. Growing wider never rewraps a clipped line, so it
	// keeps the cheap in-place path.
	if f.prev > 0 && (width < f.w || f.prev > height) {
		b.WriteString("\x1b[H\x1b[J")
		f.prev = 0
	}
	f.w = width
	if len(lines) > height {
		lines = lines[:height]
	}
	// The leading \r also clears a pending-wrap state a full-width last line
	// left behind, so the move-up counts whole rows.
	b.WriteString("\r")
	if f.prev > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", f.prev-1)
	}
	for i, s := range lines {
		s = render.Clip(s, width)
		if i == len(lines)-1 {
			// Clear-below runs from the start of the last row, before its
			// text: it erases both this row and every row a taller previous
			// frame left underneath, and running it here rather than after
			// the write keeps a full-width line's pending wrap from putting
			// the erase on top of the line's own last cell.
			b.WriteString("\x1b[J" + s)
		} else {
			b.WriteString("\x1b[2K" + s + "\r\n")
		}
	}
	io.WriteString(out, b.String())
	f.prev = len(lines)
}

// finish steps below the frame so cooked-mode output — the selection line,
// the shell's next prompt — starts on its own row. The frame itself stays on
// screen: a quit board lingering in the terminal is this surface's contract.
func (f *framePrinter) finish() {
	if f.prev == 0 {
		return
	}
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	io.WriteString(out, "\r\n")
	f.prev = 0
}
