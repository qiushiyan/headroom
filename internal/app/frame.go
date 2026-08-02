package app

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/render"
)

// framePrinter redraws a frame in place: move up over the previous frame,
// rewrite every line (\x1b[2K clears remnants of longer ones), and blank
// any rows the previous, taller frame still occupies. Lines are clipped to
// the terminal width — a wrapped line would occupy two physical rows and
// shear the move-up arithmetic.
type framePrinter struct {
	prev int
}

func (f *framePrinter) print(lines []string) {
	width, _, sizeErr := term.GetSize(int(os.Stdout.Fd()))
	var b strings.Builder
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
	os.Stdout.WriteString(b.String())
	f.prev = len(lines)
}
