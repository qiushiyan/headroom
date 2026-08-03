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
	"github.com/qiushiyan/headroom/internal/throttle"
	"github.com/qiushiyan/headroom/internal/tui"
)

const (
	defaultInterval = 2 * time.Minute
	minInterval     = 30 * time.Second
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
	th := throttle.Load(cfg.AccountsRoot)

	var list []*accountData
	var updates <-chan fetchUpdate // nil = no round in flight
	var lastRound time.Time
	nextAt := time.Now()

	// Rate-limit backoff is per account and lives in the throttle store, not
	// in this loop: one account being refused says nothing about the others,
	// and a fleet-wide multiplier punished healthy accounts for it.
	//
	// The store is re-read every round rather than held: watch runs for hours,
	// and a snapshot taken at startup cannot see claims a dashboard, --json,
	// select or check made since. Holding it would let the longest-running
	// surface spend budget those had already spent — the exact double-spend
	// the store exists to stop. Rounds never overlap, so replacing the pointer
	// here is safe.
	startRound := func() {
		th = throttle.Load(cfg.AccountsRoot)
		newList, _ := prepare(cfg, th)
		carryOver(newList, list)
		list = newList
		updates = launchFetches(ctx, cfg, list, th)
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
			if n := deferredCount(list); n > 0 {
				status = append(status, fmt.Sprintf("%d cooling down", n))
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
				_ = th.Save()
				nextAt = lastRound.Add(interval)
			} else {
				resolve(list[u.idx], u.res, th, time.Now())
			}
			draw()
		case <-tick.C:
			if updates == nil && !time.Now().Before(nextAt) {
				startRound()
			}
			draw()
		case k := <-t.Events():
			switch {
			case isCancelKey(k):
				t.Close()
				return 0
			case k == tui.Key{Kind: tui.KeyRune, Rune: 'r'}:
				// A manual refresh asks the same question sooner; it does not
				// buy exemption from the endpoint's budget. Per-account
				// eligibility is enforced in prepare, so a held-down `r`
				// re-reads and redraws without issuing a single request.
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

// carryOver keeps the previous round's observation for accounts whose fresh
// fetch hasn't landed yet, so a refresh updates in place instead of blanking.
// The observation carries *whole* — its timestamp and source included — so
// last round's numbers keep saying how old they are instead of passing as
// this round's. Header fields (label, plan, current) are always this round's.
//
// A carried observation is only kept when it beats what prepare already
// loaded from Claude Code's cache, which is usually much older.
func carryOver(newList, old []*accountData) {
	prev := make(map[string]*render.Observation, len(old))
	for _, d := range old {
		if d.View.Obs != nil {
			prev[d.Acct.Name] = d.View.Obs
		}
	}
	for _, d := range newList {
		o, ok := prev[d.Acct.Name]
		if !ok {
			continue
		}
		if d.View.Obs == nil || o.ObservedAt > d.View.Obs.ObservedAt {
			d.View.Obs = o
		}
	}
}

func deferredCount(list []*accountData) int {
	n := 0
	for _, d := range list {
		if d.View.Attempt.State == render.AttemptDeferred {
			n++
		}
	}
	return n
}
