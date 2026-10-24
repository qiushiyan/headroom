package app

// The account board: which account has headroom left, and which one bare `x`
// should target from now on. One surface, because there was only ever one
// board — a one-shot print, a picker and a watch loop were three renderings of
// it, and the two interactive ones had grown near-identical draw loops.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
)

// runAccounts is the board. Off a terminal it prints one frame and exits, so
// `headroom` in a pipe, a script or a non-interactive ssh still answers the
// question it exists to answer.
//
// layout is the presentation — the classic blocks or one row per account —
// and nothing but presentation: both layouts run the same rounds, honour
// the same claim and commit the same choice.
func runAccounts(scopes []config.Scope, layout render.Layout) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		return printBoard(scopes, layout)
	}
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err != nil || w <= 0 || h <= 0 {
		// An interactive tty that won't state its size cannot host the
		// in-place board: a multi-row redraw over guessed geometry is how
		// frames duplicate into scrollback. The one-shot print is the honest
		// rendering there, and the caption says why the picker didn't open.
		fmt.Fprintln(os.Stderr, "headroom accounts: terminal reports no size — printing the board once")
		return printBoard(scopes, layout)
	}
	return runPicker(scopes, layout)
}

// printBoard is the non-interactive rendering: fetch once, draw once, exit.
// Nothing is clipped: there is no redraw arithmetic to protect, and a line
// the terminal wraps still says everything. The terminal's width, when
// there is one, only sizes the compact layout's columns so its rows fit
// when they can.
func printBoard(scopes []config.Scope, layout render.Layout) int {
	boards := fetchBoards(scopes)
	tty := stdoutIsTTY()
	p := render.NewPalette(tty)
	width := 0
	if tty {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			width = w
		}
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	writeBoards(out, p, boards, time.Now().Unix(), layout, width)
	return 0
}

// writeBoards prints the pages in vendor order. When two vendors are printed
// each gets a heading line; with one there is no heading and the frame is
// what it always was.
func writeBoards(out io.Writer, p render.Palette, boards []board, now int64, layout render.Layout, width int) {
	for n, vb := range boards {
		if len(boards) > 1 {
			if n > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintln(out, p.Bold+vb.scope.Vendor.Title()+p.Rst)
			if layout == render.LayoutBlocks {
				fmt.Fprintln(out)
			}
		}
		b := p.Board(views(vb.list), now, layout, width)
		if vb.routing.Mode == "auto" {
			fmt.Fprintln(out, p.Dim+autoHeader(layout)+p.Rst)
			if layout == render.LayoutBlocks {
				fmt.Fprintln(out)
			}
		}
		for _, line := range b.Header {
			fmt.Fprintln(out, line)
		}
		for i, g := range b.Groups {
			if i > 0 && layout == render.LayoutBlocks {
				fmt.Fprintln(out)
			}
			for _, line := range g {
				fmt.Fprintln(out, line)
			}
		}
	}
}

// chosenLine says what enter just did, naming the bare launch that now
// targets the account.
func chosenLine(scope config.Scope, label string) string {
	if scope.Vendor == config.Codex {
		return fmt.Sprintf("→ %s — bare Codex launches (headroom launch --vendor codex, no --account) now target it", label)
	}
	return fmt.Sprintf("→ %s — bare launches (no --account) now target it", label)
}

// autoLine says what `a` just did, in the words chosenLine uses for enter.
func autoLine(scope config.Scope) string {
	if scope.Vendor == config.Codex {
		return "→ auto — bare Codex launches (headroom launch --vendor codex, no --account) now choose the least-loaded account"
	}
	return "→ auto — bare launches (no --account) now choose the least-loaded account"
}

// autoHeader is the one line a board in auto mode puts above its accounts: no
// row is the current one, and the marked row is where a launch would go now.
func autoHeader(layout render.Layout) string {
	mark := "← next"
	if layout == render.LayoutCompact {
		mark = "→"
	}
	return "bare launches are automatic — " + mark + " marks the account one would take now"
}
