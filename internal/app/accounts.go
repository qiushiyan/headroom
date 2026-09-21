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
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/refresh"
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
//
// The two reasons are counted apart because they are said apart: an account
// the vendor has blocked is not "too old", and calling it that would send the
// user to refresh figures that are perfectly current.
func notActionable(list []*accountData, now int64) (blocked, stale int) {
	for _, d := range list {
		switch {
		case d.View.Obs == nil || d.View.Actionable(now):
		case d.View.Blocked():
			blocked++
		default:
			stale++
		}
	}
	return blocked, stale
}

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
func writeBoards(out io.Writer, p render.Palette, boards []vendorBoard, now int64, layout render.Layout, width int) {
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

// page is one vendor's board: its list, its selection, its scroll position
// and its refresh schedule. A page owns its in-flight round — one list and one
// channel until that channel closes and drains — so a result can only ever
// land on the page that started the round, never on whichever page happens to
// be visible when it arrives.
type page struct {
	scope config.Scope
	st    *state.Store

	set     accounts.Set // what the current list was discovered as
	list    []*accountData
	updates <-chan refresh.Result // non-nil while this page's round is in flight
	sel     int
	selName string // survives a round that reorders or re-discovers accounts
	opened  bool   // the first visit has chosen the initial selection

	lastLocal time.Time // when the local half last ran — the floor's clock
	nextAt    time.Time // next scheduled cadence round
	armed     bool      // r pressed while floored or mid-round; fires when the floor allows
	manual    bool      // current round was asked for by hand: acknowledge on completion
	ack       string    // transient acknowledgement after a manual round
	ackUntil  time.Time
	wanted    int // accounts the last round had anything to ask about
	top       int // first body line in view when the board outgrows the terminal

	// begin starts a round: the local half, then the claim and whatever
	// fetches it permits. nil means the real thing; tests inject channels.
	begin func(ctx context.Context, pg *page) (accounts.Set, []*accountData, <-chan refresh.Result)
}

// picker is the interactive board: the pages, which one is visible, and the
// terminal. Only the visible page starts rounds; a hidden page keeps its
// figures and their age, and its deadline never wakes the loop.
type picker struct {
	pages   []*page
	visible int
	p       render.Palette
	fp      *framePrinter
	layout  render.Layout
	lastKey time.Time // presence
}

func (ui *picker) page() *page { return ui.pages[ui.visible] }

const ackTTL = 4 * time.Second

func newPage(scope config.Scope) *page {
	return &page{scope: scope, st: state.Open(scope)}
}

func runPicker(scopes []config.Scope, layout render.Layout) int {
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
		p:       render.NewPalette(true),
		fp:      &framePrinter{},
		layout:  layout,
		lastKey: time.Now(),
	}
	for _, scope := range scopes {
		ui.pages = append(ui.pages, newPage(scope))
	}
	// Every exit — enter, cancel, the deferred Close, a signal death — steps
	// below the newline-less frame through the same once-guarded path.
	t.OnClose(ui.fp.finish)
	ui.show(ctx, 0)
	ui.draw()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		switch ui.step(ctx, tick.C, t.Events()) {
		case stepCancel:
			t.Close()
			return 1
		case stepChoose:
			pg := ui.page()
			chosen := pg.list[pg.sel]
			t.Close()
			// Enter records this page's vendor's current account and nothing
			// else: the other vendor's .current is not touched.
			if err := pg.set.SetCurrent(chosen.Acct); err != nil {
				fmt.Fprintf(os.Stderr, "headroom accounts: %v\n", err)
				return 1
			}
			fmt.Println(chosenLine(pg.scope, chosen.View.Label))
			return 0
		}
		ui.draw()
	}
}

// stepOutcome is what one turn of the board's loop asks of its caller.
type stepOutcome int

const (
	stepContinue stepOutcome = iota
	stepCancel
	stepChoose // enter on a row of the visible page
)

// step is one turn of the board's loop: one refresh result, one tick or one
// key. It is the only place results are routed and rounds are started, which
// is what the page model's two promises rest on — a result is applied to the
// page whose channel delivered it, whichever page is visible, and only the
// visible page is ever asked whether a round is due.
func (ui *picker) step(ctx context.Context, tick <-chan time.Time, keys <-chan tui.Key) stepOutcome {
	// The vendor set is closed at two, so the loop receives from every
	// page's channel by name. A page with no round in flight has a nil
	// channel, which never delivers.
	var second <-chan refresh.Result
	if len(ui.pages) > 1 {
		second = ui.pages[1].updates
	}
	select {
	case u, open := <-ui.pages[0].updates:
		ui.pages[0].receive(u, open, time.Now())
	case u, open := <-second:
		ui.pages[1].receive(u, open, time.Now())
	case <-tick:
		if pg := ui.page(); pg.due(ui.lastKey) {
			// A round due because it was armed is still the user's ask.
			pg.startRound(ctx, pg.armed)
		}
	case k := <-keys:
		ui.lastKey = time.Now()
		pg := ui.page()
		switch {
		case k.Kind == tui.KeyUp || k == tui.Key{Kind: tui.KeyRune, Rune: 'k'}:
			pg.move(-1)
		case k.Kind == tui.KeyDown || k == tui.Key{Kind: tui.KeyRune, Rune: 'j'}:
			pg.move(1)
		case k.Kind == tui.KeyTab:
			ui.show(ctx, (ui.visible+1)%len(ui.pages))
		case k == tui.Key{Kind: tui.KeyRune, Rune: 'r'}:
			pg.refresh(ctx)
		case isCancelKey(k):
			return stepCancel
		case k.Kind == tui.KeyEnter:
			if len(pg.list) > 0 {
				return stepChoose
			}
		}
	}
	return stepContinue
}

// chosenLine says what enter just did, naming the bare launch that now
// targets the account.
func chosenLine(scope config.Scope, label string) string {
	if scope.Vendor == config.Codex {
		return fmt.Sprintf("→ %s — bare Codex launches (headroom launch --vendor codex, no --account) now target it", label)
	}
	return fmt.Sprintf("→ %s — bare launches (no --account) now target it", label)
}

// show makes a page visible. A page seen for the first time has nothing to
// draw, so its first round starts now; one whose deadline passed while it was
// hidden is simply due at the next tick. The first draw of a page shows the
// current account selected; after that the selection follows the user.
func (ui *picker) show(ctx context.Context, i int) {
	ui.visible = i
	pg := ui.pages[i]
	if pg.list == nil && pg.updates == nil {
		pg.startRound(ctx, false)
	}
	if !pg.opened {
		pg.opened = true
		for j, d := range pg.list {
			if d.View.Current {
				pg.sel, pg.selName = j, d.Acct.Name
			}
		}
	}
}

// receive applies one result of this page's own round, or closes the round
// out when its channel has drained. Only this receiving path updates facts.
func (pg *page) receive(u refresh.Result, open bool, now time.Time) {
	if open {
		resolve(pg.list[u.Index], u)
		return
	}
	pg.updates = nil
	pg.schedule()
	if pg.manual {
		// The user asked; the answer is what changed, said once and briefly —
		// a claim persisting past its moment is how the old "refresh armed"
		// read as being ignored.
		pg.ack, pg.ackUntil = pg.ackString(now), now.Add(ackTTL)
		pg.manual = false
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
func (ui *page) startRound(ctx context.Context, manual bool) {
	begin := ui.begin
	if begin == nil {
		begin = func(ctx context.Context, pg *page) (accounts.Set, []*accountData, <-chan refresh.Result) {
			p := prepare(pg.scope, pg.st)
			return p.set, p.list, launchFetches(ctx, p.list, pg.st)
		}
	}
	set, list, updates := begin(ctx, ui)
	ui.set, ui.list = set, list
	ui.restoreSelection()
	ui.lastLocal = time.Now()
	ui.armed = false
	ui.manual = manual
	// Scheduling distinguishes an idle account set from a deferred refresh.
	ui.wanted = 0
	for _, d := range list {
		if d.Request != nil {
			ui.wanted++
		}
	}
	ui.updates = updates
}

// ackString is the one-line answer to a manual refresh, composed after the
// round lands: either everything obtainable was obtained, or the budget is
// holding some account back and the soonest retry is named. Per-row detail
// (which account, why) is already on the rows via their attempt captions.
func (ui *page) ackString(now time.Time) string {
	var next time.Time
	for _, d := range ui.list {
		if d.View.Attempt.State == accountstate.AttemptOK || d.View.Attempt.State == accountstate.AttemptNoLimits {
			continue
		}
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
func (ui *page) schedule() {
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
		next = now.Add(ui.scope.RequestSpacing())
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
func (ui *page) due(lastKey time.Time) bool {
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
	return time.Since(lastKey) < presenceWindow
}

// refresh is the `r` key: ask now. The claim — never this loop — decides per
// account whether a request may leave, so the round runs immediately and a
// still-cooling account comes back annotated with when it can next be asked,
// rather than the key silently re-arming the cadence. The two states that
// defer it both remember the press (`r` is never a no-op): a round already in
// flight only carries the accounts eligible at its start, and a press inside
// the floor is deferred spawn hygiene, not budget politeness — the queued
// round fires the moment the floor passes, unattended included.
func (ui *page) refresh(ctx context.Context) {
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
func (ui *page) refreshNow(now time.Time) bool {
	return ui.updates == nil && !now.Before(ui.lastLocal.Add(refreshFloor))
}

func (ui *page) move(delta int) {
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
func (ui *page) restoreSelection() {
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
	pg := ui.page()
	// One geometry reading builds and prints the frame: a resize landing
	// mid-draw hits the next tick, never a frame windowed to one screen and
	// printed to another.
	w, h := ui.fp.geometry()
	b := ui.p.Board(views(pg.list), now.Unix(), ui.layout, w)
	// The tab bar is part of the header: it ranks with the column headings
	// when the terminal is too short for everything. One page has no bar, and
	// its frame is what it always was.
	var header []string
	if bar := ui.tabBar(); bar != "" {
		header = append(header, "  "+bar)
		if ui.layout == render.LayoutBlocks {
			header = append(header, "")
		}
	}
	for _, line := range b.Header {
		header = append(header, "  "+line)
	}
	var body []string
	selStart, selEnd := 0, 0
	for i, g := range b.Groups {
		if i == pg.sel {
			selStart = len(body)
		}
		for j, line := range g {
			prefix := "  "
			if i == pg.sel && j == 0 {
				prefix = ui.p.Bold + "▶ " + ui.p.Rst
			}
			body = append(body, prefix+line)
		}
		if i == pg.sel {
			selEnd = len(body)
		}
		if i < len(b.Groups)-1 && ui.layout == render.LayoutBlocks {
			body = append(body, "")
		}
	}
	footer := []string{"", ui.p.Dim + ui.status(now) + ui.p.Rst}
	// Fit the board to the terminal — framePrinter's move-up arithmetic
	// cannot survive a frame taller than the screen, and its own clamp cuts
	// blindly from the tail.
	var keepHeader, keepFooter bool
	var view int
	pg.top, view, keepHeader, keepFooter = boardWindow(pg.top, selStart, selEnd, len(body), len(header), len(footer), h)
	body = body[pg.top : pg.top+view]
	if !keepHeader {
		header = nil
	}
	if !keepFooter {
		footer = nil
	}
	frame := append(header, body...)
	ui.fp.print(append(frame, footer...), w, h)
}

// tabBar names every page and marks the visible one. "" with a single page.
func (ui *picker) tabBar() string {
	if len(ui.pages) < 2 {
		return ""
	}
	parts := make([]string, len(ui.pages))
	for i, pg := range ui.pages {
		title := " " + pg.scope.Vendor.Title() + " "
		if i == ui.visible {
			parts[i] = ui.p.Rev + ui.p.Bold + title + ui.p.Rst
		} else {
			parts[i] = ui.p.Dim + title + ui.p.Rst
		}
	}
	if ui.p.Rev == "" {
		// No colour to carry the mark: brackets do.
		for i, pg := range ui.pages {
			parts[i] = " " + pg.scope.Vendor.Title() + " "
			if i == ui.visible {
				parts[i] = "[" + pg.scope.Vendor.Title() + "]"
			}
		}
	}
	return strings.Join(parts, " ")
}

// boardWindow decides what a chooser needs most on a screen of h rows: the
// selected block scrolls into view whole so enter never commits an account
// the user cannot see; the header (the compact layout's column headings —
// none in the block layout) and the footer (warnings, refresh state) render
// whenever a body row can stand beside them; and on a terminal too short for
// all of it the selection outranks the header, which outranks the footer —
// a row of figures is unreadable without its headings, and the refresh
// countdown is the one thing a chooser can do without. Pure so the
// arithmetic is table-testable.
func boardWindow(top, selStart, selEnd, bodyLen, headerLen, footerLen, h int) (newTop, view int, keepHeader, keepFooter bool) {
	switch {
	case bodyLen+headerLen+footerLen <= h:
		return 0, bodyLen, true, true
	case h-headerLen-footerLen >= 1:
		view := h - headerLen - footerLen
		return fitTop(top, selStart, selEnd, bodyLen, view), view, true, true
	case h-headerLen >= 1:
		view := h - headerLen
		return fitTop(top, selStart, selEnd, bodyLen, view), view, true, false
	default:
		view := min(h, bodyLen)
		return fitTop(top, selStart, selEnd, bodyLen, view), view, false, false
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
	pg := ui.page()
	var parts []string
	switch {
	case pg.updates != nil && pg.armed:
		parts = append(parts, "refreshing… · queued")
	case pg.updates != nil:
		parts = append(parts, "refreshing…")
	case pg.armed:
		parts = append(parts, "queued · refresh in "+until(pg.lastLocal.Add(refreshFloor), now))
	case pg.ack != "" && now.Before(pg.ackUntil):
		parts = append(parts, pg.ack)
	case pg.nextAt.IsZero():
		// no round has completed yet
	case time.Since(ui.lastKey) >= presenceWindow:
		parts = append(parts, "paused — r to refresh")
	default:
		parts = append(parts, "next refresh in "+until(pg.nextAt, now))
	}
	if pg.scope.PrimaryRelocated {
		// launch refuses the primary under a relocated home (the board is
		// describing a tree a bare launch would not use); the board must say
		// so before someone picks it.
		parts = append(parts, "HEADROOM_HOME set — primary launches refuse")
	}
	for _, d := range pg.list {
		if !d.View.Current {
			continue
		}
		// An inherited value that disagrees with `← current` would re-route a
		// bare launch; managed launches neutralize it, and the board must not
		// silently rely on that. The classifier is launch's, so this note
		// and the environment actually built cannot disagree. A value that
		// names a discovered extra's dir stays silent, though: it means only
		// "this shell lives inside a managed session", which is this
		// machine's ordinary environment — check reports it, the board does
		// not caption the normal case.
		if tgt, err := launch.For(pg.scope.Vendor, d.Acct.ConfigDir); err == nil {
			if v, conflicting := tgt.Conflicts(os.Environ()); conflicting && !pg.set.KnownExtraDir(v) {
				parts = append(parts, "ambient "+pg.scope.Env().HomeVar+" neutralized")
			}
		}
		break
	}
	blocked, stale := notActionable(pg.list, now.Unix())
	if blocked > 0 {
		// Positive evidence from the vendor, not age: these accounts are not
		// grounds for a choice whatever their percentages say.
		parts = append(parts, fmt.Sprintf("%d blocked by the vendor", blocked))
	}
	if stale > 0 {
		// The picker's whole purpose is choosing on current headroom. An
		// account is grounds for a choice only when it is usable, its figures
		// are current, and no window has rolled over since they were taken.
		parts = append(parts, fmt.Sprintf("%d too old to pick on", stale))
	}
	hints := "↑/↓ move · enter select · r refresh · esc cancel"
	if len(ui.pages) > 1 {
		hints = "tab switch · " + hints
	}
	parts = append(parts, hints)
	return strings.Join(parts, " · ")
}

func until(t, now time.Time) string {
	d := max(t.Sub(now), 0)
	return d.Round(time.Second).String()
}
