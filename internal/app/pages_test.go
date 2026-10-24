package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/tui"
	"github.com/qiushiyan/headroom/internal/usage"
)

// injectedPage is a page whose rounds are channels the test holds, so a
// result can be delivered at a chosen moment. begun counts the rounds it
// started.
func injectedPage(t *testing.T, vendor config.Vendor, names ...string) (*page, chan refresh.Result, *int) {
	t.Helper()
	scope := config.Scope{Vendor: vendor, AccountsRoot: t.TempDir()}
	ch := make(chan refresh.Result, 8)
	begun := 0
	pg := newPage(scope)
	pg.begin = func(_ context.Context, pg *page) (board, <-chan refresh.Result) {
		begun++
		return injectedBoard(pg, names...), ch
	}
	return pg, ch, &begun
}

// injectedBoard is a round's board under the page's own scope and store, its
// rows as prepare leaves them: usable, and each with a request to make.
func injectedBoard(pg *page, names ...string) board {
	b := board{scope: pg.scope, st: pg.st, set: accounts.Set{Scope: pg.scope}, list: make([]*accountData, len(names))}
	for i, n := range names {
		b.list[i] = &accountData{
			Acct:    accounts.Account{Scope: pg.scope, Name: n},
			View:    accountstate.Facts{Vendor: pg.scope.Vendor, Label: n, Health: accountstate.HealthOK, Attempt: accountstate.Attempt{State: accountstate.AttemptPending}},
			Request: &refresh.Candidate{},
		}
	}
	return b
}

func observed(percent int) refresh.Result {
	return refresh.Result{
		Attempt:     accountstate.Attempt{State: accountstate.AttemptOK},
		Observation: &accountstate.Observation{Rows: []usage.Row{{Label: "weekly", Percent: percent}}, ObservedAt: time.Now().Unix()},
	}
}

// Pages own their rounds, through the loop's own dispatch. A result that
// arrives after the person switched away lands on the page that started the
// round — the same index on the now-visible page is somebody else's account —
// and a tick never starts a round on a hidden page, however overdue it is.
func TestPagesOwnTheirRounds(t *testing.T) {
	claude, claudeCh, claudeBegun := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com")
	codex, codexCh, codexBegun := injectedPage(t, config.Codex, "x0@x.com", "x1@x.com")
	ui := &picker{pages: []*page{claude, codex}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	tick := make(chan time.Time, 1)
	keys := make(chan tui.Key, 1)

	ui.show(ctx, 0)
	if *claudeBegun != 1 || *codexBegun != 0 {
		t.Fatalf("opening: claude began %d, codex began %d — only the visible page may start a round", *claudeBegun, *codexBegun)
	}

	// Tab away while Claude Code's round is still in flight…
	keys <- tui.Key{Kind: tui.KeyTab}
	ui.step(ctx, tick, keys)
	if ui.visible != 1 || *codexBegun != 1 {
		t.Fatalf("tab: visible %d, codex began %d — a first visit starts that page's round", ui.visible, *codexBegun)
	}
	// …then its result lands, addressed to index 1, while Codex is visible.
	r := observed(61)
	r.Index = 1
	claudeCh <- r
	ui.step(ctx, tick, keys)

	if got := claude.list[1].View.Obs; got == nil || got.Rows[0].Percent != 61 {
		t.Errorf("the originating page did not get its result: %+v", claude.list[1].View)
	}
	for i, d := range codex.list {
		if d.View.Obs != nil || d.View.Attempt.State != accountstate.AttemptPending {
			t.Errorf("codex[%d] was touched by Claude Code's round: %+v", i, d.View)
		}
	}

	// The round closes on its own page, which schedules itself.
	close(claudeCh)
	ui.step(ctx, tick, keys)
	if claude.updates != nil || claude.nextAt.IsZero() {
		t.Errorf("round not closed out on its page: updates=%v nextAt=%v", claude.updates, claude.nextAt)
	}
	if codex.updates == nil {
		t.Error("closing Claude Code's round ended Codex's")
	}

	// Claude Code is hidden and long overdue; a tick asks only the visible
	// page, whose own round is still in flight — so nothing starts.
	claude.nextAt = time.Now().Add(-time.Hour)
	tick <- time.Now()
	ui.step(ctx, tick, keys)
	if *claudeBegun != 1 || *codexBegun != 1 {
		t.Errorf("a tick started a round off the visible page: claude %d, codex %d", *claudeBegun, *codexBegun)
	}

	// Back on its page, the passed deadline is simply due at the next tick.
	close(codexCh)
	ui.step(ctx, tick, keys)
	keys <- tui.Key{Kind: tui.KeyTab}
	ui.step(ctx, tick, keys)
	if *claudeBegun != 1 {
		t.Errorf("returning to a page restarted it (%d rounds)", *claudeBegun)
	}
	claude.lastLocal = time.Now().Add(-time.Hour)
	tick <- time.Now()
	ui.step(ctx, tick, keys)
	if *claudeBegun != 2 {
		t.Errorf("a deadline that passed while hidden did not fire once visible (%d rounds)", *claudeBegun)
	}
}

// Enter and cancel leave the loop to its caller; enter on an empty page is
// not a choice.
func TestStepOutcomes(t *testing.T) {
	pg, _, _ := injectedPage(t, config.Codex, "x0@x.com")
	empty, _, _ := injectedPage(t, config.Claude)
	ui := &picker{pages: []*page{pg, empty}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	keys := make(chan tui.Key, 1)
	ui.show(ctx, 0)
	for _, c := range []struct {
		key  tui.Key
		want stepOutcome
	}{
		{tui.Key{Kind: tui.KeyDown}, stepContinue},
		{tui.Key{Kind: tui.KeyEnter}, stepChoose},
		{tui.Key{Kind: tui.KeyEsc}, stepCancel},
	} {
		keys <- c.key
		if got := ui.step(ctx, nil, keys); got != c.want {
			t.Errorf("key %+v: outcome %v, want %v", c.key, got, c.want)
		}
	}
	ui.show(ctx, 1)
	keys <- tui.Key{Kind: tui.KeyEnter}
	if got := ui.step(ctx, nil, keys); got != stepContinue {
		t.Errorf("enter on an empty page: outcome %v", got)
	}
}

// Each page keeps its own selection, and the tab bar appears only when there
// is more than one page.
func TestPagesKeepTheirOwnSelectionAndTheTabBar(t *testing.T) {
	claude, _, _ := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com")
	codex, _, _ := injectedPage(t, config.Codex, "x0@x.com", "x1@x.com")
	ui := &picker{pages: []*page{claude, codex}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	ui.show(ctx, 0)
	ui.page().move(1)
	ui.show(ctx, 1)
	if ui.page().sel != 0 {
		t.Errorf("Codex inherited Claude Code's selection: %d", ui.page().sel)
	}
	ui.show(ctx, 0)
	if ui.page().sel != 1 || ui.page().selName != "c1@x.com" {
		t.Errorf("Claude Code's selection was lost: %d %q", ui.page().sel, ui.page().selName)
	}

	if bar := ui.tabBar(); !strings.Contains(bar, "[Claude Code]") || !strings.Contains(bar, "Codex") || strings.Contains(bar, "[Codex]") {
		t.Errorf("tab bar = %q", bar)
	}
	ui.visible = 1
	if bar := ui.tabBar(); !strings.Contains(bar, "[Codex]") {
		t.Errorf("tab bar = %q", bar)
	}
	if !strings.Contains(ui.status(time.Now()), "tab switch") {
		t.Errorf("footer does not teach tab: %q", ui.status(time.Now()))
	}

	solo := &picker{pages: []*page{claude}, p: render.NewPalette(false), lastKey: time.Now()}
	if solo.tabBar() != "" || strings.Contains(solo.status(time.Now()), "tab switch") {
		t.Error("one vendor must render today's frame: no tab bar, no tab hint")
	}
}

// A page's first visit opens on the row enter would re-confirm: the current
// account, or under auto the account a launch would take now. Opening on row
// 0 instead would make enter-without-moving pin whatever sorts first, and
// silently re-route every bare launch. Disabling show's first-visit choice
// must fail both halves.
func TestAPageOpensOnTheRowEnterWouldConfirm(t *testing.T) {
	ctx := context.Background()
	pinned, _, _ := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com", "c2@x.com")
	rows := pinned.begin
	pinned.begin = func(ctx context.Context, pg *page) (board, <-chan refresh.Result) {
		b, ch := rows(ctx, pg)
		b.list[1].View.Current = true
		return b, ch
	}
	(&picker{pages: []*page{pinned}, p: render.NewPalette(false), lastKey: time.Now()}).show(ctx, 0)
	if pinned.sel != 1 || pinned.selName != "c1@x.com" {
		t.Errorf("pinned: opened on row %d (%q), want the current account's row 1", pinned.sel, pinned.selName)
	}

	// Under auto no row is current: the least-loaded one is marked next.
	f := boardFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 50, 10, time.Minute)
	f.observe("a@x.com", 40, 10, time.Minute)
	f.observe("b@x.com", 0, 10, time.Minute)
	auto := newPage(f.cfg)
	(&picker{pages: []*page{auto}, p: render.NewPalette(false), lastKey: time.Now()}).show(ctx, 0)
	_, next := marks(auto.list)
	if len(next) != 1 || next[0] == auto.list[0].Acct.Name {
		t.Fatalf("fixture: next %v must be one row other than the first", next)
	}
	if got := auto.list[auto.sel].Acct.Name; got != next[0] || auto.selName != next[0] {
		t.Errorf("auto: opened on %s (%q), want the next row %s", got, auto.selName, next[0])
	}
}

// Rediscovery between rounds can reorder the list — an account dir added or
// removed — and the cursor stays on the account, not the index: a user about
// to press enter would otherwise commit whichever account slid under it.
// Making restoreSelection a no-op must fail this.
func TestTheSelectionFollowsItsAccountAcrossARediscovery(t *testing.T) {
	pg, ch, _ := injectedPage(t, config.Claude, "a@x.com", "b@x.com", "c@x.com")
	ui := &picker{pages: []*page{pg}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	ui.show(ctx, 0)
	pg.move(1)
	close(ch)
	ui.step(ctx, nil, nil)

	// The next round discovers one more account, sorted ahead of the chosen one.
	pg.begin = func(_ context.Context, pg *page) (board, <-chan refresh.Result) {
		return injectedBoard(pg, "a@x.com", "aa@x.com", "b@x.com", "c@x.com"), make(chan refresh.Result)
	}
	pg.startRound(ctx, false)
	if got := pg.list[pg.sel].Acct.Name; got != "b@x.com" || pg.sel != 2 {
		t.Errorf("the cursor moved to %s (row %d) when a rediscovery reordered the list; it was on b@x.com", got, pg.sel)
	}
}

// The footer names a vendor block as a block — not as figures too old.
func TestFooterCountsBlockedApartFromStale(t *testing.T) {
	now := time.Now().Unix()
	fresh := func(a usage.Allowance, age int64) *accountData {
		return &accountData{View: accountstate.Facts{Vendor: config.Codex, Health: accountstate.HealthOK,
			Obs: &accountstate.Observation{Rows: []usage.Row{{Percent: 1}}, Allowance: a, ObservedAt: now - age}}}
	}
	list := []*accountData{
		fresh(usage.Allowance{State: usage.AllowanceBlocked}, 1),
		fresh(usage.Allowance{State: usage.AllowanceAllowed, BlockedFeatures: []string{"code review"}}, 1),
		fresh(usage.Allowance{State: usage.AllowanceAllowed}, 100000),
		fresh(usage.Allowance{State: usage.AllowanceBad}, 1),
	}
	if blocked, stale := notActionable(list, now); blocked != 1 || stale != 1 {
		t.Errorf("blocked=%d stale=%d, want 1 and 1", blocked, stale)
	}
}
