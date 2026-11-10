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
	"github.com/qiushiyan/headroom/internal/round"
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
	pg := newPage(scope, BoardDeps{Round: LiveRound(true), Place: quietPlace()})
	pg.begin = func(_ context.Context, pg *page) (*round.Round, <-chan round.Result) {
		begun++
		r := injectedRound(pg, injectedAccounts(pg, names...))
		return r, r.Feed(ch)
	}
	return pg, ch, &begun
}

// injectedAccounts are accounts as prepare leaves usable ones: healthy, each
// with a request pending.
func injectedAccounts(pg *page, names ...string) []accountstate.Account {
	accts := make([]accountstate.Account, len(names))
	for i, n := range names {
		accts[i] = accountstate.Account{
			Acct: accounts.Account{Scope: pg.scope, Name: n},
			View: accountstate.Facts{Vendor: pg.scope.Vendor, Label: n, Health: accountstate.HealthOK, Attempt: accountstate.Attempt{State: accountstate.AttemptPending}},
		}
	}
	return accts
}

// injectedRound is a round over those accounts under the page's own scope and
// store.
func injectedRound(pg *page, accts []accountstate.Account) *round.Round {
	return round.Of(accounts.Set{Scope: pg.scope}, pg.st, accts)
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
	clock := newFakeClock()
	claude.now, codex.now = clock.Now, clock.Now
	ui := &picker{pages: []*page{claude, codex}, p: render.NewPalette(false), lastKey: clock.Now(), now: clock.Now}
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

	if got := claude.accounts()[1].View.Obs; got == nil || got.Rows[0].Percent != 61 {
		t.Errorf("the originating page did not get its result: %+v", claude.accounts()[1].View)
	}
	for i, d := range codex.accounts() {
		if d.View.Obs != nil || d.View.Attempt.State != accountstate.AttemptPending {
			t.Errorf("codex[%d] was touched by Claude Code's round: %+v", i, d.View)
		}
	}

	// The round closes on its own page, which schedules itself.
	close(claudeCh)
	ui.step(ctx, tick, keys)
	if said := claude.refreshStatus(ui.lastKey); !strings.HasPrefix(said, "next refresh in ") {
		t.Errorf("round not closed out and scheduled on its page: %q", said)
	}
	if said := codex.refreshStatus(ui.lastKey); said != "refreshing…" {
		t.Errorf("closing Claude Code's round ended Codex's: %q", said)
	}

	// Claude Code is hidden and long overdue; a tick asks only the visible
	// page, whose own round is still in flight — so nothing starts.
	clock.advance(time.Hour)
	tick <- time.Now()
	ui.step(ctx, tick, keys)
	if *claudeBegun != 1 || *codexBegun != 1 {
		t.Errorf("a tick started a round off the visible page: claude %d, codex %d", *claudeBegun, *codexBegun)
	}

	// Back on Claude Code's page, the other way round: Codex's round is still
	// in flight, and its result and its close land on Codex's page, never the
	// visible one. The loop receives from each page's channel by name, so
	// each direction is its own line in step.
	keys <- tui.Key{Kind: tui.KeyTab}
	ui.step(ctx, tick, keys)
	if *claudeBegun != 1 {
		t.Errorf("returning to a page restarted it (%d rounds)", *claudeBegun)
	}
	r = observed(17)
	codexCh <- r
	ui.step(ctx, tick, keys)
	if got := codex.accounts()[0].View.Obs; got == nil || got.Rows[0].Percent != 17 {
		t.Errorf("a hidden page did not get its result: %+v", codex.accounts()[0].View)
	}
	if got := claude.accounts()[0].View.Obs; got != nil {
		t.Errorf("Codex's result landed on the visible page: %+v", got)
	}
	close(codexCh)
	ui.step(ctx, tick, keys)
	if said := codex.refreshStatus(ui.lastKey); !strings.HasPrefix(said, "next refresh in ") {
		t.Errorf("a hidden page's round was not closed out and scheduled on it: %q", said)
	}

	// On its page, the deadline that passed while hidden is simply due at the
	// next tick.
	tick <- time.Now()
	ui.step(ctx, tick, keys)
	if *claudeBegun != 2 {
		t.Errorf("a deadline that passed while hidden did not fire once visible (%d rounds)", *claudeBegun)
	}
}

// Quitting is the one key that leaves the loop; every other key keeps the
// board open, on a page with accounts or without.
func TestStepOutcomes(t *testing.T) {
	pg, _, _ := injectedPage(t, config.Codex, "x0@x.com")
	empty, _, _ := injectedPage(t, config.Claude)
	ui := &picker{pages: []*page{pg, empty}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	keys := make(chan tui.Key, 1)
	ui.show(ctx, 0)
	for _, c := range []struct {
		key  tui.Key
		quit bool
	}{
		{tui.Key{Kind: tui.KeyDown}, false},
		{tui.Key{Kind: tui.KeyEnter}, false},
		{tui.Key{Kind: tui.KeyEsc}, true},
		{tui.Key{Kind: tui.KeyRune, Rune: 'q'}, true},
	} {
		keys <- c.key
		if quit := ui.step(ctx, nil, keys); quit != c.quit {
			t.Errorf("key %+v: quit %v, want %v", c.key, quit, c.quit)
		}
	}
	ui.show(ctx, 1)
	keys <- tui.Key{Kind: tui.KeyEnter}
	if ui.step(ctx, nil, keys) {
		t.Error("enter on an empty page quit the board")
	}
}

// Each page keeps its own scroll, the tab bar appears only when there is more
// than one page, and the footer offers the arrows only on a board taller than
// the terminal.
func TestPagesKeepTheirOwnScrollAndTheTabBar(t *testing.T) {
	claude, _, _ := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com")
	codex, _, _ := injectedPage(t, config.Codex, "x0@x.com", "x1@x.com")
	ui := &picker{pages: []*page{claude, codex}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	keys := make(chan tui.Key, 1)
	ui.show(ctx, 0)
	keys <- tui.Key{Kind: tui.KeyDown}
	ui.step(ctx, nil, keys)
	ui.show(ctx, 1)
	if ui.page().top != 0 {
		t.Errorf("Codex inherited Claude Code's scroll: %d", ui.page().top)
	}
	ui.show(ctx, 0)
	if ui.page().top != 1 {
		t.Errorf("Claude Code's scroll was lost: %d", ui.page().top)
	}

	if bar := ui.tabBar(); !strings.Contains(bar, "[Claude Code]") || !strings.Contains(bar, "Codex") || strings.Contains(bar, "[Codex]") {
		t.Errorf("tab bar = %q", bar)
	}
	ui.visible = 1
	if bar := ui.tabBar(); !strings.Contains(bar, "[Codex]") {
		t.Errorf("tab bar = %q", bar)
	}
	if !strings.Contains(ui.status(time.Now(), false), "tab switch") {
		t.Errorf("footer does not teach tab: %q", ui.status(time.Now(), false))
	}
	if footer := ui.status(time.Now(), false); strings.Contains(footer, "scroll") || !strings.Contains(footer, "q quit") {
		t.Errorf("a board that fits: footer = %q", footer)
	}
	if footer := ui.status(time.Now(), true); !strings.Contains(footer, "↑/↓ scroll") {
		t.Errorf("a board taller than the terminal: footer = %q", footer)
	}

	solo := &picker{pages: []*page{claude}, p: render.NewPalette(false), lastKey: time.Now()}
	if solo.tabBar() != "" || strings.Contains(solo.status(time.Now(), false), "tab switch") {
		t.Error("one vendor must render today's frame: no tab bar, no tab hint")
	}
}

// The footer names a vendor block as a block — not as figures too old.
func TestFooterCountsBlockedApartFromStale(t *testing.T) {
	now := time.Now().Unix()
	fresh := func(a usage.Allowance, age int64) accountstate.Account {
		return accountstate.Account{View: accountstate.Facts{Vendor: config.Codex, Health: accountstate.HealthOK,
			Obs: &accountstate.Observation{Rows: []usage.Row{{Percent: 1}}, Allowance: a, ObservedAt: now - age}}}
	}
	list := []accountstate.Account{
		fresh(usage.Allowance{State: usage.AllowanceBlocked}, 1),
		fresh(usage.Allowance{State: usage.AllowanceAllowed, BlockedFeatures: []string{"code review"}}, 1),
		fresh(usage.Allowance{State: usage.AllowanceAllowed}, 100000),
		fresh(usage.Allowance{State: usage.AllowanceBad}, 1),
	}
	if blocked, stale := notActionable(list, now); blocked != 1 || stale != 1 {
		t.Errorf("blocked=%d stale=%d, want 1 and 1", blocked, stale)
	}
}
