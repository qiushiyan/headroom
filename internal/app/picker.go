package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/tui"
)

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
		case stepAuto:
			pg := ui.page()
			t.Close()
			// Like enter, this records the visible page's vendor's routing and
			// nothing else, and starts no session.
			if err := pg.set.SetAuto(); err != nil {
				fmt.Fprintf(os.Stderr, "headroom accounts: %v\n", err)
				return 1
			}
			fmt.Println(autoLine(pg.scope))
			return 0
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
	stepAuto   // a: bare launches of the visible page's vendor choose for themselves
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
		case k == tui.Key{Kind: tui.KeyRune, Rune: 'a'}:
			return stepAuto
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
			// Under auto there is no current account; the row a launch would
			// take is the one worth opening on.
			if d.View.Current || d.View.Next {
				pg.sel, pg.selName = j, d.Acct.Name
			}
		}
	}
}

func (ui *picker) draw() {
	now := time.Now()
	pg := ui.page()
	pg.markAt(now)
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
	if pg.routing.Mode == "auto" {
		header = append(header, "  "+ui.p.Dim+autoHeader(ui.layout)+ui.p.Rst)
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
	if pg.scope.PrimaryLaunchRefused() {
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
		// silently rely on that. The classifier is launch's own, so this note
		// and the launch's notice cannot disagree about which values go
		// unsaid.
		if tgt, err := launch.For(pg.scope.Vendor, d.Acct.ConfigDir); err == nil {
			if _, unexplained := launch.Unexplained(tgt, pg.set, os.Environ()); unexplained {
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
	hints := "↑/↓ move · enter select · a auto · r refresh · esc cancel"
	if len(ui.pages) > 1 {
		hints = "tab switch · " + hints
	}
	parts = append(parts, hints)
	return strings.Join(parts, " · ")
}
