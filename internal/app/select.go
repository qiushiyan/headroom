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
	"github.com/qiushiyan/headroom/internal/throttle"
	"github.com/qiushiyan/headroom/internal/tui"
)

// staleCount is how many accounts are showing figures too old to describe
// current headroom. The picker warns rather than hides: an old number is
// useful context, but choosing on it is the mistake worth naming.
func staleCount(list []*accountData, now int64) int {
	n := 0
	for _, d := range list {
		if d.View.Obs != nil && !d.View.Fresh(now) {
			n++
		}
	}
	return n
}

// runSelect shows the dashboard as an interactive picker: bars fill in as
// fetches land, enter writes .current so bare `x` (and the x-select
// wrapper) target the chosen account.
func runSelect(cfg config.Config) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "headroom select: requires an interactive terminal")
		return 1
	}

	th := throttle.Load(cfg.AccountsRoot)
	list, _ := prepare(cfg, th)
	updates := launchFetches(context.Background(), cfg, list, th)
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
		hint := "↑/↓ move · enter select · esc cancel"
		if n := staleCount(list, now); n > 0 {
			// The picker's whole purpose is choosing on current headroom.
			// Numbers too old to mean that are shown but must not be passed
			// off as grounds for the choice.
			hint = fmt.Sprintf("%d account(s) showing stale figures · %s", n, hint)
		}
		lines = append(lines, "", p.Dim+hint+p.Rst)
		fp.print(lines)
	}
	draw()

	for {
		select {
		case u, ok := <-updates:
			if !ok {
				updates = nil // all fetches resolved; stop selecting on it
				_ = th.Save()
				continue
			}
			resolve(list[u.idx], u.res, th, time.Now())
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
