package app

// watch is the dashboard on a deliberately lazy loop. The two update rates
// are decoupled on purpose: countdowns re-render every second from cached
// data at zero network cost, while fetch rounds run on a generous interval
// — the endpoint is undocumented and vendor-tolerated at human rates, so
// watch must never out-poll a human. One round in flight at a time, a hard
// interval floor, and backoff on 429 keep that promise; `r` refreshes by
// hand, `q` quits.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/tui"
)

const (
	defaultInterval = 2 * time.Minute
	minInterval     = 30 * time.Second
	maxBackoff      = 8 // interval multiplier cap while rate-limited
)

func runWatch(cfg config.Config, args []string) int {
	interval, ok := watchInterval(args, os.Stderr)
	if !ok {
		return 2
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "headroom watch: requires an interactive terminal")
		return 1
	}

	t, err := tui.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom watch: %v\n", err)
		return 1
	}
	defer t.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := render.NewPalette(true)
	fp := &framePrinter{}

	var list []*accountData
	var updates <-chan fetchUpdate // nil = no round in flight
	var lastRound time.Time
	backoff := 1
	nextAt := time.Now()

	startRound := func() {
		newList := prepare(cfg)
		carryOver(newList, list)
		list = newList
		updates = launchFetches(ctx, cfg, list)
	}

	draw := func() {
		now := time.Now()
		labelWidth := render.LabelWidth(views(list))
		var lines []string
		for i, d := range list {
			for _, line := range p.AccountBlock(d.View, now.Unix(), labelWidth) {
				lines = append(lines, "  "+line)
			}
			if i < len(list)-1 {
				lines = append(lines, "")
			}
		}
		var status []string
		if updates != nil {
			status = append(status, "refreshing…")
		} else {
			if !lastRound.IsZero() {
				status = append(status, fmt.Sprintf("refreshed %s ago", now.Sub(lastRound).Round(time.Second)))
			}
			status = append(status, fmt.Sprintf("next in %s", time.Until(nextAt).Round(time.Second)))
			if backoff > 1 {
				status = append(status, "backing off (rate limited)")
			}
		}
		status = append(status, "r refresh · q quit")
		lines = append(lines, "", p.Dim+strings.Join(status, " · ")+p.Rst)
		fp.print(lines)
	}

	startRound()
	draw()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case u, chOpen := <-updates:
			if !chOpen {
				updates = nil
				lastRound = time.Now()
				if sawRateLimit(list) {
					backoff = min(backoff*2, maxBackoff)
				} else {
					backoff = 1
				}
				nextAt = lastRound.Add(interval * time.Duration(backoff))
			} else {
				resolve(list[u.idx], u.res)
			}
			draw()
		case <-tick.C:
			if updates == nil && !time.Now().Before(nextAt) {
				startRound()
			}
			draw()
		case ev := <-t.Events():
			switch ev {
			case tui.EventCancel:
				t.Close()
				return 0
			case tui.EventRefresh:
				if updates == nil {
					startRound()
					draw()
				}
			}
		}
	}
}

// watchInterval parses watch's one flag. The floor is not negotiable from
// the command line: politeness toward an undocumented endpoint is part of
// the design, not a preference.
func watchInterval(args []string, errw io.Writer) (time.Duration, bool) {
	interval := defaultInterval
	for i := 0; i < len(args); i++ {
		var val string
		switch {
		case args[i] == "--interval" && i+1 < len(args):
			i++
			val = args[i]
		case strings.HasPrefix(args[i], "--interval="):
			val = strings.TrimPrefix(args[i], "--interval=")
		default:
			fmt.Fprintf(errw, "headroom watch: unexpected argument %q (want --interval <duration>)\n", args[i])
			return 0, false
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			fmt.Fprintf(errw, "headroom watch: bad interval %q: %v\n", val, err)
			return 0, false
		}
		if d < minInterval {
			fmt.Fprintf(errw, "headroom watch: interval floor is %s\n", minInterval)
			return 0, false
		}
		interval = d
	}
	return interval, true
}

// carryOver keeps the previous round's bars for accounts whose fresh fetch
// hasn't landed yet, so a refresh updates in place instead of blanking back
// to "fetching…". Only resolved rows carry; header fields (label, plan,
// current) are always this round's.
func carryOver(newList, old []*accountData) {
	prev := make(map[string]render.AccountView, len(old))
	for _, d := range old {
		prev[d.Acct.Name] = d.View
	}
	for _, d := range newList {
		if d.View.Status != render.StatusPending {
			continue
		}
		if v, ok := prev[d.Acct.Name]; ok && v.Status == render.StatusRows {
			d.View.Status = render.StatusRows
			d.View.Rows = v.Rows
		}
	}
}

func sawRateLimit(list []*accountData) bool {
	for _, d := range list {
		if d.View.Status == render.StatusHTTPError && d.View.HTTPCode == 429 {
			return true
		}
	}
	return false
}
