package app

// The account board: which account has headroom left, and which one bare `x`
// should target from now on. One surface, because there was only ever one
// board — a one-shot print, a picker and a watch loop were three renderings of
// it, and the two interactive ones had grown near-identical draw loops.
//
// Refresh cadence is eligibility itself: there is no interval to pick, because
// asking sooner is refused by the endpoint's budget and asking later would
// leave the board saying less than it could. What replaces watch's "the user
// chose to run a long-lived command" is presence — a picker left open on a
// spare tab must not poll for hours with nobody looking, which would be the
// background daemon this design deliberately does not have, wearing a TUI.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tui"
)

// presenceWindow is how long after the last keypress the board keeps
// refreshing itself. Past it the loop goes quiet until a key lands.
const presenceWindow = 5 * time.Minute

// isCancelKey is the shared "get me out" chord set: esc, q, ctrl-c, ctrl-d.
func isCancelKey(k tui.Key) bool {
	return k.Kind == tui.KeyEsc ||
		k == tui.Key{Kind: tui.KeyRune, Rune: 'q'} ||
		k == tui.Key{Kind: tui.KeyCtrl, Rune: 'c'} ||
		k == tui.Key{Kind: tui.KeyCtrl, Rune: 'd'}
}

// notActionable counts accounts whose displayed figures are not grounds for a
// choice — the account isn't usable, its numbers are too old, or a limit
// window has rolled over since they were taken. The picker warns rather than
// hides: old numbers are useful context, but choosing on them is the mistake
// worth naming.
func notActionable(list []*accountData, now int64) int {
	n := 0
	for _, d := range list {
		if d.View.Obs != nil && !d.View.Actionable(now) {
			n++
		}
	}
	return n
}

// runAccounts is the board. Off a terminal it prints one frame and exits, so
// `headroom` in a pipe, a script or a non-interactive ssh still answers the
// question it exists to answer.
func runAccounts(cfg config.Config) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		return printBoard(cfg)
	}
	return runPicker(cfg)
}

// printBoard is the non-interactive rendering: fetch once, draw once, exit.
func printBoard(cfg config.Config) int {
	st := state.Open(cfg.AccountsRoot)
	list, _ := prepare(cfg, st)
	for u := range launchFetches(context.Background(), cfg, list, st) {
		resolve(list[u.idx], u, time.Now())
	}
	p := render.NewPalette(stdoutIsTTY())
	now := time.Now().Unix()
	labelWidth := render.LabelWidth(views(list))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for i, d := range list {
		if i > 0 {
			fmt.Fprintln(out)
		}
		for _, line := range p.AccountBlock(d.View, now, labelWidth) {
			fmt.Fprintln(out, line)
		}
	}
	return 0
}

// picker is the interactive board's mutable state.
type picker struct {
	cfg config.Config
	st  *state.Store
	p   render.Palette
	fp  *framePrinter

	list    []*accountData
	updates <-chan fetchUpdate // non-nil while a round is in flight
	sel     int
	selName string // survives a round that reorders or re-discovers accounts

	lastRound time.Time
	nextAt    time.Time
	armed     bool      // r pressed inside a quiet period
	wanted    int       // accounts the last round had anything to ask about
	lastKey   time.Time // presence
}

func runPicker(cfg config.Config) int {
	t, err := tui.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom accounts: %v\n", err)
		return 1
	}
	defer t.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancelling on the way out stops in-flight fetches from outliving the
	// picker; their claims are already durable, so nothing is double-spent.
	defer cancel()

	ui := &picker{
		cfg:     cfg,
		st:      state.Open(cfg.AccountsRoot),
		p:       render.NewPalette(true),
		fp:      &framePrinter{},
		lastKey: time.Now(),
	}
	ui.startRound(ctx)
	// The first draw shows the current account selected; after that the
	// selection follows the user, not the discovery order.
	for i, d := range ui.list {
		if d.View.Current {
			ui.sel = i
			ui.selName = d.Acct.Name
		}
	}
	ui.draw()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case u, open := <-ui.updates:
			if !open {
				ui.updates = nil
				ui.lastRound = time.Now()
				ui.schedule()
			} else {
				resolve(ui.list[u.idx], u, time.Now())
			}
			ui.draw()
		case <-tick.C:
			if ui.due() {
				ui.startRound(ctx)
			}
			ui.draw()
		case k := <-t.Events():
			ui.lastKey = time.Now()
			switch {
			case k.Kind == tui.KeyUp || k == tui.Key{Kind: tui.KeyRune, Rune: 'k'}:
				ui.move(-1)
			case k.Kind == tui.KeyDown || k == tui.Key{Kind: tui.KeyRune, Rune: 'j'}:
				ui.move(1)
			case k == tui.Key{Kind: tui.KeyRune, Rune: 'r'}:
				ui.refresh(ctx)
			case isCancelKey(k):
				t.Close()
				return 1
			case k.Kind == tui.KeyEnter:
				chosen := ui.list[ui.sel]
				t.Close()
				if err := accounts.SetCurrent(cfg, chosen.Acct.Name); err != nil {
					fmt.Fprintf(os.Stderr, "headroom accounts: %v\n", err)
					return 1
				}
				fmt.Printf("→ %s — bare x now targets it\n", chosen.View.Label)
				return 0
			}
			ui.draw()
		}
	}
}

// startRound re-reads everything and claims what it may. The store is re-read
// every round rather than held: the picker can sit open for hours, and a
// snapshot taken at startup cannot see claims another surface made since.
func (ui *picker) startRound(ctx context.Context) {
	list, _ := prepare(ui.cfg, ui.st)
	ui.list = list
	ui.restoreSelection()
	ui.armed = false
	// Counted before the claim, which clears the flag on every account it
	// denies: what schedule needs to know is whether anything was worth asking
	// about at all, not how that asking went.
	ui.wanted = 0
	for _, d := range list {
		if d.WantsFetch {
			ui.wanted++
		}
	}
	ui.updates = launchFetches(ctx, ui.cfg, list, ui.st)
}

// schedule sets the next round for the moment the first account becomes
// eligible again — the soonest instant at which asking could return anything
// new, and therefore the only cadence worth having. There is no interval to
// configure: sooner is refused by the endpoint's budget, later would leave the
// board saying less than it could.
func (ui *picker) schedule() {
	now := time.Now()
	snap := ui.st.Load()
	var next time.Time
	for _, d := range ui.list {
		if e := snap.NextEligible(d.Key, now); e.After(now) && (next.IsZero() || e.Before(next)) {
			next = e
		}
	}
	// The floor is a floor, never a ceiling: a round whose claims all failed
	// records no eligibility at all, and without it the loop would retry as
	// fast as the ticker runs.
	if floor := now.Add(refreshFloor); next.IsZero() || next.Before(floor) {
		next = floor
	}
	// Unless nothing was asking in the first place. With every account logged
	// out or its token aged out there is no eligibility to wait for, and the
	// floor would become the cadence — spawning `claude auth status` per
	// account twice a minute to re-learn the same answer. Poll at the rate a
	// logged-out account can plausibly change instead.
	if ui.wanted == 0 {
		next = now.Add(presenceWindow)
	}
	ui.nextAt = next
}

const refreshFloor = 30 * time.Second

// due reports whether a round should start now. Presence is part of the
// question: an unattended board stops asking, and says so.
func (ui *picker) due() bool {
	if ui.updates != nil || ui.nextAt.IsZero() || time.Now().Before(ui.nextAt) {
		return false
	}
	return ui.armed || time.Since(ui.lastKey) < presenceWindow
}

// refresh is the `r` key. It asks the same question sooner; it does not buy
// exemption from the endpoint's budget, which is enforced by the claim and not
// by this loop. When nothing is eligible yet, the request is remembered rather
// than dropped — the round fires the moment the budget allows, including from
// an unattended board.
func (ui *picker) refresh(ctx context.Context) {
	if ui.updates != nil {
		// A round is already running, but it only carries the accounts that
		// were eligible when it started. Arming rather than dropping the key
		// keeps the promise that `r` is never a no-op.
		ui.armed = true
		return
	}
	if ui.nextAt.IsZero() || !time.Now().Before(ui.nextAt) {
		ui.startRound(ctx)
		return
	}
	ui.armed = true
}

func (ui *picker) move(delta int) {
	n := ui.sel + delta
	if n < 0 || n >= len(ui.list) {
		return
	}
	ui.sel = n
	ui.selName = ui.list[n].Acct.Name
}

// restoreSelection re-finds the selected account by name. Rediscovery can
// reorder the list — an account dir added or removed between rounds — and a
// preserved index would silently move the cursor to a different account under
// a user about to press enter.
func (ui *picker) restoreSelection() {
	if ui.selName == "" {
		return
	}
	for i, d := range ui.list {
		if d.Acct.Name == ui.selName {
			ui.sel = i
			return
		}
	}
	if ui.sel >= len(ui.list) {
		ui.sel = len(ui.list) - 1
	}
	if ui.sel < 0 {
		ui.sel = 0
	}
}

func (ui *picker) draw() {
	now := time.Now()
	labelWidth := render.LabelWidth(views(ui.list))
	var lines []string
	for i, d := range ui.list {
		for j, line := range ui.p.AccountBlock(d.View, now.Unix(), labelWidth) {
			prefix := "  "
			if i == ui.sel && j == 0 {
				prefix = ui.p.Bold + "▶ " + ui.p.Rst
			}
			lines = append(lines, prefix+line)
		}
		if i < len(ui.list)-1 {
			lines = append(lines, "")
		}
	}
	lines = append(lines, "", ui.p.Dim+ui.status(now)+ui.p.Rst)
	ui.fp.print(lines)
}

func (ui *picker) status(now time.Time) string {
	var parts []string
	switch {
	case ui.updates != nil:
		parts = append(parts, "refreshing…")
	case ui.armed:
		parts = append(parts, "refresh armed · in "+until(ui.nextAt, now))
	case ui.nextAt.IsZero():
		// no round has completed yet
	case time.Since(ui.lastKey) >= presenceWindow:
		parts = append(parts, "paused — r to refresh")
	default:
		parts = append(parts, "next refresh in "+until(ui.nextAt, now))
	}
	if n := notActionable(ui.list, now.Unix()); n > 0 {
		// The picker's whole purpose is choosing on current headroom. An
		// account is grounds for a choice only when it is usable, its figures
		// are current, and no window has rolled over since they were taken.
		parts = append(parts, fmt.Sprintf("%d showing figures you should not pick on", n))
	}
	parts = append(parts, "↑/↓ move · enter select · r refresh · esc cancel")
	return strings.Join(parts, " · ")
}

func until(t, now time.Time) string {
	d := t.Sub(now)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
