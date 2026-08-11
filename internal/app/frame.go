package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/render"
)

// framePrinter redraws a frame in place: move up over the previous frame,
// rewrite every line (\x1b[2K clears remnants of longer ones), and blank
// any rows the previous, taller frame still occupies. The move-up arithmetic
// holds only while every printed line is one physical row still standing
// where it was printed, so three guards protect it. Lines are clipped to the
// terminal width — a wrapped line would occupy two rows. A frame is cut to
// the terminal height — the cursor cannot move above the screen, so a taller
// frame would scroll on every redraw and push a copy of its top into
// scrollback each time (the duplicated-board bug). And a resize that could
// have rewrapped the previous frame's rows — the terminal narrowed, or
// shrank below the frame — falsifies prev, so the printer repaints from a
// cleared screen instead of moving up a count the resize just broke.
var errUnknownSize = errors.New("terminal size unknown")

type framePrinter struct {
	prev int
	w    int // width the previous frame was printed at

	// Test seams; nil means the real stdout terminal.
	out  io.Writer
	size func() (w, h int, err error)
}

func (f *framePrinter) print(lines []string) {
	getSize := f.size
	if getSize == nil {
		getSize = func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }
	}
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	width, height, sizeErr := getSize()
	if sizeErr == nil && (width <= 0 || height <= 0) {
		// Some ptys report 0×0 without an error; an unknown size must not
		// masquerade as a resize (prev >= 0 would clear-repaint every frame).
		sizeErr = errUnknownSize
	}
	var b strings.Builder
	if sizeErr == nil {
		// prev counts physical rows, and only these two resizes can change
		// how many rows the old frame occupies: narrowing rewraps them in a
		// reflowing terminal, and a height below the frame leaves no room to
		// move back over it. Growing wider never rewraps a clipped line, so
		// it keeps the cheap in-place path.
		if f.prev > 0 && (width < f.w || f.prev >= height) {
			b.WriteString("\x1b[H\x1b[J")
			f.prev = 0
		}
		f.w = width
		// The frame plus the cursor's resting row must fit the screen; the
		// last height-1 guard keeps a one-row terminal from cutting to zero
		// and printing nothing forever.
		if max := height - 1; max >= 1 && len(lines) > max {
			lines = lines[:max]
		}
	}
	if f.prev > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", f.prev)
	}
	for _, s := range lines {
		if sizeErr == nil {
			s = render.Clip(s, width)
		}
		b.WriteString("\x1b[2K" + s + "\r\n")
	}
	if extra := f.prev - len(lines); extra > 0 {
		for range extra {
			b.WriteString("\x1b[2K\r\n")
		}
		fmt.Fprintf(&b, "\x1b[%dA", extra)
	}
	io.WriteString(out, b.String())
	f.prev = len(lines)
}
