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
	"github.com/qiushiyan/headroom/internal/usage"
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

// knownExtraDir adapts the board's list to accounts.KnownExtraDir.
func knownExtraDir(list []*accountData, dir string) bool {
	accts := make([]accounts.Account, len(list))
	for i, d := range list {
		accts[i] = d.Acct
	}
	return accounts.KnownExtraDir(accts, dir)
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
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err != nil || w <= 0 || h <= 0 {
		// An interactive tty that won't state its size cannot host the
		// in-place board: a multi-row redraw over guessed geometry is how
		// frames duplicate into scrollback. The one-shot print is the honest
		// rendering there, and the caption says why the picker didn't open.
		fmt.Fprintln(os.Stderr, "headroom accounts: terminal reports no size — printing the board once")
		return printBoard(cfg)
	}
	return runPicker(cfg)
}

// printBoard is the non-interactive rendering: fetch once, draw once, exit.
func printBoard(cfg config.Config) int {
	st := state.Open(cfg.AccountsRoot)
	list, _, _ := prepare(cfg, st)
	for u := range launchFetches(context.Background(), cfg, list, st, 1) {
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
	round   int64              // stamps fetch updates; a stale round's update is dropped
	sel     int
	selName string // survives a round that reorders or re-discovers accounts

	lastLocal time.Time // when the local half last ran — the floor's clock
	nextAt    time.Time // next scheduled cadence round
	armed     bool      // r pressed while floored or mid-round; fires when the floor allows
	manual    bool      // current round was asked for by hand: acknowledge on completion
	ack       string    // transient acknowledgement after a manual round
	ackUntil  time.Time
	wanted    int       // accounts the last round had anything to ask about
	lastKey   time.Time // presence
	top       int       // first body line in view when the board outgrows the terminal
}

const ackTTL = 4 * time.Second

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
	// Every exit — enter, cancel, the deferred Close, a signal death — steps
	// below the newline-less frame through the same once-guarded path.
	t.OnClose(ui.fp.finish)
	ui.startRound(ctx, false)
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
				ui.schedule()
				if ui.manual {
					// The user asked; the answer is what changed, said once
					// and briefly — a claim persisting past its moment is how
					// the old "refresh armed" read as being ignored.
					now := time.Now()
					ui.ack, ui.ackUntil = ui.ackString(now), now.Add(ackTTL)
					ui.manual = false
				}
			} else {
				ui.apply(u, time.Now())
			}
			ui.draw()
		case <-tick.C:
			if ui.due() {
				// A round due because it was armed is still the user's ask.
				ui.startRound(ctx, ui.armed)
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

// startRound runs one full round: a local half — re-read everything free
// (health, discovery, credentials, `.current`, the store's replay) — and a
// budget half, the claim plus whatever fetches it permits. The two halves are
// named because only the first needs the floor: it costs a `claude auth
// status` spawn per account, which a held key must not spin, while the second
// is priced by the claim itself. They always run together — a round that
// skipped the claim would freeze the schedule (nothing would recompute
// nextAt), and one that skipped the local half would fetch against a stale
// registry. The store is re-read every round rather than held: the picker can
// sit open for hours, and a snapshot taken at startup cannot see claims
// another surface made since.
func (ui *picker) startRound(ctx context.Context, manual bool) {
	list, _, _ := prepare(ui.cfg, ui.st)
	ui.list = list
	ui.restoreSelection()
	ui.lastLocal = time.Now()
	ui.armed = false
	ui.manual = manual
	ui.round++
	// Counted before the claim, which clears the flag on every account it
	// denies: what schedule needs to know is whether anything was worth asking
	// about at all, not how that asking went.
	ui.wanted = 0
	for _, d := range list {
		if d.WantsFetch {
			ui.wanted++
		}
	}
	ui.updates = launchFetches(ctx, ui.cfg, list, ui.st, ui.round)
}

// apply lands one fetch update on its view. The round stamp is what makes
// the positional idx safe by construction rather than by discipline: idx
// addresses the list its own round was built over, and an update surviving
// from a superseded round must never index into a rebuilt one.
func (ui *picker) apply(u fetchUpdate, now time.Time) {
	if u.round != ui.round {
		return
	}
	resolve(ui.list[u.idx], u, now)
}

// ackString is the one-line answer to a manual refresh, composed after the
// round lands: either everything obtainable was obtained, or the budget is
// holding some account back and the soonest retry is named. Per-row detail
// (which account, why) is already on the rows via their attempt captions.
func (ui *picker) ackString(now time.Time) string {
	var next time.Time
	for _, d := range ui.list {
		if at := d.View.Attempt.NextEligibleAt; at > now.Unix() {
			t := time.Unix(at, 0)
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}
	if next.IsZero() {
		return "refreshed · all current"
	}
	return "refreshed · usage in " + until(next, now)
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
	// account twice a minute to re-learn the same answer. The request spacing
	// is the right rate for that too: it is the slowest cadence at which this
	// board ever tells the user something new, and it must stay *under* the
	// presence window, which is measured from the same instant and would
	// otherwise have expired by the time the round came due — a poll scheduled
	// past its own gate never fires at all.
	if ui.wanted == 0 {
		next = now.Add(usage.RequestSpacing)
	}
	ui.nextAt = next
}

// refreshFloor is the minimum spacing between rounds, and it exists for the
// local half's spawn cost alone — never as budget arithmetic, which is the
// claim's. No scheduled cadence can hit it (every scheduled round is at least
// a request spacing away); only a hand-delivered `r` can, so treating it as
// an interval to tune would turn spawn hygiene into a polling rate.
const refreshFloor = 30 * time.Second

// due reports whether a round should start now. Presence is part of the
// question: an unattended board stops asking, and says so. An armed round —
// one the user asked for — fires the moment the floor allows, attended or
// not: it was asked for.
func (ui *picker) due() bool {
	if ui.updates != nil {
		return false
	}
	now := time.Now()
	if ui.armed {
		return !now.Before(ui.lastLocal.Add(refreshFloor))
	}
	if ui.nextAt.IsZero() || now.Before(ui.nextAt) {
		return false
	}
	return time.Since(ui.lastKey) < presenceWindow
}

// refresh is the `r` key: ask now. The claim — never this loop — decides per
// account whether a request may leave, so the round runs immediately and a
// still-cooling account comes back annotated with when it can next be asked,
// rather than the key silently re-arming the cadence. The two states that
// defer it both remember the press (`r` is never a no-op): a round already in
// flight only carries the accounts eligible at its start, and a press inside
// the floor is deferred spawn hygiene, not budget politeness — the queued
// round fires the moment the floor passes, unattended included.
func (ui *picker) refresh(ctx context.Context) {
	ui.ack, ui.ackUntil = "", time.Time{} // a new ask supersedes the old answer
	if !ui.refreshNow(time.Now()) {
		ui.armed = true
		return
	}
	ui.startRound(ctx, true)
}

// refreshNow reports whether `r` may run a round this instant, as opposed to
// arming one. Split from refresh so the dispatch is testable without running
// prepare's subprocess spawns.
func (ui *picker) refreshNow(now time.Time) bool {
	return ui.updates == nil && !now.Before(ui.lastLocal.Add(refreshFloor))
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
	var body []string
	selStart, selEnd := 0, 0
	for i, d := range ui.list {
		if i == ui.sel {
			selStart = len(body)
		}
		for j, line := range ui.p.AccountBlock(d.View, now.Unix(), labelWidth) {
			prefix := "  "
			if i == ui.sel && j == 0 {
				prefix = ui.p.Bold + "▶ " + ui.p.Rst
			}
			body = append(body, prefix+line)
		}
		if i == ui.sel {
			selEnd = len(body)
		}
		if i < len(ui.list)-1 {
			body = append(body, "")
		}
	}
	footer := []string{"", ui.p.Dim + ui.status(now) + ui.p.Rst}
	// Fit the board to the terminal — framePrinter's move-up arithmetic
	// cannot survive a frame taller than the screen, and its own clamp cuts
	// blindly from the tail. One geometry reading builds and prints the
	// frame: a resize landing mid-draw hits the next tick, never a frame
	// windowed to one screen and printed to another.
	w, h := ui.fp.geometry()
	var keepFooter bool
	var view int
	ui.top, view, keepFooter = boardWindow(ui.top, selStart, selEnd, len(body), len(footer), h)
	body = body[ui.top : ui.top+view]
	if !keepFooter {
		footer = nil
	}
	ui.fp.print(append(body, footer...), w, h)
}

// boardWindow decides what a chooser needs most on a screen of h rows: the
// selected block scrolls into view whole so enter never commits an account
// the user cannot see, the footer (warnings, refresh state) renders whenever
// a body row can stand beside it, and on a terminal too short for both, the
// selection outranks the footer. Pure so the arithmetic is table-testable.
func boardWindow(top, selStart, selEnd, bodyLen, footerLen, h int) (newTop, view int, keepFooter bool) {
	switch view := h - footerLen; {
	case bodyLen+footerLen <= h:
		return 0, bodyLen, true
	case view >= 1:
		return fitTop(top, selStart, selEnd, bodyLen, view), view, true
	default:
		view = h
		if view > bodyLen {
			view = bodyLen
		}
		return fitTop(top, selStart, selEnd, bodyLen, view), view, false
	}
}

// fitTop scrolls the board's viewport to keep the selected block whole in
// view; when the block itself outgrows the viewport, its first line wins.
func fitTop(top, selStart, selEnd, total, view int) int {
	if selEnd > top+view {
		top = selEnd - view
	}
	if selStart < top {
		top = selStart
	}
	if top > total-view {
		top = total - view
	}
	if top < 0 {
		top = 0
	}
	return top
}

// status is the board's one footer line. Clause order is clip order —
// render.Clip cuts from the right, so what must survive a narrow terminal
// comes first: the refresh state (the user's most recent question), then the
// two warnings, then the hints as the designated casualty.
func (ui *picker) status(now time.Time) string {
	var parts []string
	switch {
	case ui.updates != nil && ui.armed:
		parts = append(parts, "refreshing… · queued")
	case ui.updates != nil:
		parts = append(parts, "refreshing…")
	case ui.armed:
		parts = append(parts, "queued · refresh in "+until(ui.lastLocal.Add(refreshFloor), now))
	case ui.ack != "" && now.Before(ui.ackUntil):
		parts = append(parts, ui.ack)
	case ui.nextAt.IsZero():
		// no round has completed yet
	case time.Since(ui.lastKey) >= presenceWindow:
		parts = append(parts, "paused — r to refresh")
	default:
		parts = append(parts, "next refresh in "+until(ui.nextAt, now))
	}
	if ui.cfg.PrimaryRelocated {
		// launch refuses the primary under a relocated home (the board is
		// describing a tree bare `claude` would not use); the board must say
		// so before someone picks it.
		parts = append(parts, "HEADROOM_HOME set — primary launches refuse")
	}
	for _, d := range ui.list {
		if !d.View.Current {
			continue
		}
		// An inherited value that disagrees with `← x` would re-route a bare
		// `claude`; managed launches neutralize it, and the board must not
		// silently rely on that. The classifier is launch's, so this note
		// and the environment actually built cannot disagree. A value that
		// names a discovered extra's dir stays silent, though: it means only
		// "this shell lives inside a managed session", which is this
		// machine's ordinary environment — check reports it, the board does
		// not caption the normal case.
		if tgt, err := target(d.Acct); err == nil {
			if v, conflicting := tgt.Conflicts(os.Environ()); conflicting && !knownExtraDir(ui.list, v) {
				parts = append(parts, "ambient CLAUDE_CONFIG_DIR neutralized")
			}
		}
		break
	}
	if n := notActionable(ui.list, now.Unix()); n > 0 {
		// The picker's whole purpose is choosing on current headroom. An
		// account is grounds for a choice only when it is usable, its figures
		// are current, and no window has rolled over since they were taken.
		parts = append(parts, fmt.Sprintf("%d too old to pick on", n))
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
