package app

import (
	"context"
	"fmt"
	"os"
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
	updates := launchFetches(context.Background(), cfg, list)
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

	fp := &framePrinter{}
	draw := func() {
		now := time.Now().Unix()
		labelWidth := render.LabelWidth(views(list))
		var lines []string
		for i, d := range list {
			for j, line := range p.AccountBlock(d.View, now, labelWidth) {
				prefix := "  "
				if i == sel && j == 0 {
					prefix = p.Bold + "▶ " + p.Rst
				}
				lines = append(lines, prefix+line)
			}
			if i < len(list)-1 {
				lines = append(lines, "")
			}
		}
		lines = append(lines, "", p.Dim+"↑/↓ move · enter select · esc cancel"+p.Rst)
		fp.print(lines)
	}
	draw()

	for {
		select {
		case u, ok := <-updates:
			if !ok {
				updates = nil // all fetches resolved; stop selecting on it
				continue
			}
			resolve(list[u.idx], u.res)
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
