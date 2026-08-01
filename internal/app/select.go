package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/tui"
)

// runSelect shows the dashboard as an interactive picker: bars fill in as
// fetches land, enter writes .current so bare `x` (and the x-select
// wrapper) target the chosen account.
func runSelect(cfg config.Config) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "headroom select: requires an interactive terminal")
		return 1
	}

	list := prepare(cfg)
	updates := launchFetches(cfg, list)
	p := render.NewPalette(true)

	t, err := tui.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom select: %v\n", err)
		return 1
	}
	defer t.Close()

	sel := 0
	for i, d := range list {
		if d.View.Current {
			sel = i
		}
	}

	// Frames are redrawn in place: move up over the previous frame and
	// rewrite each line (\x1b[2K clears remnants of longer lines).
	prevLines := 0
	draw := func() {
		var b strings.Builder
		if prevLines > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", prevLines)
		}
		now := time.Now().Unix()
		lines := 0
		emit := func(s string) {
			b.WriteString("\x1b[2K" + s + "\r\n")
			lines++
		}
		for i, d := range list {
			for j, line := range p.AccountBlock(d.View, now) {
				prefix := "  "
				if i == sel && j == 0 {
					prefix = p.Bold + "▶ " + p.Rst
				}
				emit(prefix + line)
			}
			if i < len(list)-1 {
				emit("")
			}
		}
		emit("")
		emit(p.Dim + "↑/↓ move · enter select · esc cancel" + p.Rst)
		os.Stdout.WriteString(b.String())
		prevLines = lines
	}
	draw()

	for {
		select {
		case _, ok := <-updates:
			if !ok {
				updates = nil // all fetches resolved; stop selecting on it
				continue
			}
			draw()
		case ev := <-t.Events():
			switch ev {
			case tui.EventUp:
				if sel > 0 {
					sel--
					draw()
				}
			case tui.EventDown:
				if sel < len(list)-1 {
					sel++
					draw()
				}
			case tui.EventCancel:
				t.Close()
				return 1
			case tui.EventSelect:
				chosen := list[sel]
				t.Close()
				if err := accounts.SetCurrent(cfg, chosen.Acct.Name); err != nil {
					fmt.Fprintf(os.Stderr, "headroom select: %v\n", err)
					return 1
				}
				fmt.Printf("→ %s — bare x now targets it\n", chosen.View.Label)
				return 0
			}
		}
	}
}
