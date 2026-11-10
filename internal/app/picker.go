package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/tui"
)

// isCancelKey is the shared "get me out" chord set: esc, q, ctrl-c, ctrl-d.
// On the board it closes the board; in the other pickers it cancels a choice.
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
func notActionable(list []accountstate.Account, now int64) (blocked, stale int) {
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
//
// The board is read-only: no key on it records routing or starts a session.
// Its enter once pinned the selected row, and a stray enter on a board opened
// only to read it took every later bare launch out of automatic placement. An
// account is chosen by naming it for one launch (`launch --account`, a shell's
// x-<name>); every other launch is placed.
type picker struct {
	pages   []*page
	visible int
	p       render.Palette
	fp      *framePrinter
	layout  render.Layout
	lastKey time.Time // presence

	// now is the board's clock, the one its pages read too; nil is the wall
	// clock.
	now func() time.Time
}

func (ui *picker) page() *page { return ui.pages[ui.visible] }

func (ui *picker) clock() time.Time {
	if ui.now != nil {
		return ui.now()
	}
	return time.Now()
}

func runPicker(std Streams, term Terminal, scopes []config.Scope, layout render.Layout, deps BoardDeps) error {
	t, err := tui.Open()
	if err != nil {
		return err
	}
	defer t.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancelling on the way out stops in-flight fetches from outliving the
	// picker; their claims are already durable, so nothing is double-spent.
	defer cancel()

	ui := &picker{
		p:       render.NewPalette(true),
		fp:      &framePrinter{out: std.Out, size: term.Size},
		layout:  layout,
		lastKey: time.Now(),
	}
	for _, scope := range scopes {
		ui.pages = append(ui.pages, newPage(scope, deps))
	}
	// Every exit — a quit key, the deferred Close, a signal death — steps
	// below the newline-less frame through the same once-guarded path.
	t.OnClose(ui.fp.finish)
	ui.show(ctx, 0)
	ui.draw()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for !ui.step(ctx, tick.C, t.Events()) {
		ui.draw()
	}
	// Closing a board that could not have recorded anything is its ordinary
	// end, so it exits 0.
	t.Close()
	return nil
}

// step is one turn of the board's loop: one refresh result, one tick or one
// key, and whether that key quit the board. It is the only place results are
// routed and rounds are started, which is what the page model's two promises
// rest on — a result is applied to the page whose channel delivered it,
// whichever page is visible, and only the visible page is ever asked whether
// a round is due.
func (ui *picker) step(ctx context.Context, tick <-chan time.Time, keys <-chan tui.Key) (quit bool) {
	// The vendor set is closed at two, so the loop receives from every
	// page's channel by name. A page with no round in flight has a nil
	// channel, which never delivers.
	var second <-chan round.Result
	if len(ui.pages) > 1 {
		second = ui.pages[1].updates
	}
	select {
	case u, open := <-ui.pages[0].updates:
		ui.pages[0].receive(u, open)
	case u, open := <-second:
		ui.pages[1].receive(u, open)
	case <-tick:
		ui.page().tick(ctx, ui.lastKey)
	case k := <-keys:
		ui.lastKey = ui.clock()
		pg := ui.page()
		switch {
		case k.Kind == tui.KeyUp || k == tui.Key{Kind: tui.KeyRune, Rune: 'k'}:
			pg.top--
		case k.Kind == tui.KeyDown || k == tui.Key{Kind: tui.KeyRune, Rune: 'j'}:
			// Past the end is clamped by the next draw, which knows the height.
			pg.top++
		case k.Kind == tui.KeyTab:
			ui.show(ctx, (ui.visible+1)%len(ui.pages))
		case k == tui.Key{Kind: tui.KeyRune, Rune: 'r'}:
			pg.refresh(ctx)
		case isCancelKey(k):
			return true
		}
	}
	return false
}

// show makes a page visible. A page seen for the first time has nothing to
// draw, so its first round starts now; one whose deadline passed while it was
// hidden is simply due at the next tick. Each page keeps its own scroll.
func (ui *picker) show(ctx context.Context, i int) {
	ui.visible = i
	pg := ui.pages[i]
	if pg.round == nil && pg.updates == nil {
		pg.startRound(ctx, false)
	}
}

func (ui *picker) draw() {
	now := ui.clock()
	pg := ui.page()
	pg.markAt(now)
	// One geometry reading builds and prints the frame: a resize landing
	// mid-draw hits the next tick, never a frame windowed to one screen and
	// printed to another.
	w, h := ui.fp.geometry()
	accts := pg.accounts()
	b := ui.p.Board(views(accts), now.Unix(), ui.layout, w)
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
	for _, line := range b.Body() {
		if line != "" {
			line = "  " + line
		}
		body = append(body, line)
	}
	// Fit the board to the terminal — framePrinter's move-up arithmetic
	// cannot survive a frame taller than the screen, and its own clamp cuts
	// blindly from the tail. The footer is two lines whatever it says.
	var keepHeader, keepFooter bool
	var view int
	pg.top, view, keepHeader, keepFooter = boardWindow(pg.top, len(body), len(header), 2, h)
	var footer []string
	if keepFooter {
		footer = []string{"", ui.p.Dim + ui.status(now, view < len(body)) + ui.p.Rst}
	}
	body = body[pg.top : pg.top+view]
	if !keepHeader {
		header = nil
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
// two warnings, then the hints as the designated casualty. scrolls says the
// board is taller than the terminal, the one case the arrows do anything.
func (ui *picker) status(now time.Time, scrolls bool) string {
	pg := ui.page()
	var parts []string
	if s := pg.refreshStatus(ui.lastKey); s != "" {
		parts = append(parts, s)
	}
	if pg.scope.PrimaryLaunchRefused() {
		// launch refuses the primary under a relocated home (the board is
		// describing a tree a bare launch would not use); the board must say
		// so before someone picks it.
		parts = append(parts, "HEADROOM_HOME set — primary launches refuse")
	}
	accts := pg.accounts()
	blocked, stale := notActionable(accts, now.Unix())
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
	var hints []string
	if len(ui.pages) > 1 {
		hints = append(hints, "tab switch")
	}
	if scrolls {
		hints = append(hints, "↑/↓ scroll")
	}
	hints = append(hints, "r refresh", "q quit")
	parts = append(parts, hints...)
	return strings.Join(parts, " · ")
}
