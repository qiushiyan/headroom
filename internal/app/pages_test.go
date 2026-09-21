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
	"github.com/qiushiyan/headroom/internal/state"
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
	pg := &page{scope: scope, st: state.Open(scope)}
	pg.begin = func(ctx context.Context, _ *page) ([]*accountData, <-chan refresh.Result) {
		begun++
		list := make([]*accountData, len(names))
		for i, n := range names {
			list[i] = &accountData{
				Acct:    accounts.Account{Scope: scope, Name: n},
				View:    accountstate.Facts{Vendor: vendor, Label: n, Health: accountstate.HealthOK, Attempt: accountstate.Attempt{State: accountstate.AttemptPending}},
				Request: &refresh.Candidate{},
			}
		}
		return list, ch
	}
	return pg, ch, &begun
}

func observed(percent int) refresh.Result {
	return refresh.Result{
		Attempt:     accountstate.Attempt{State: accountstate.AttemptOK},
		Observation: &accountstate.Observation{Rows: []usage.Row{{Label: "weekly", Percent: percent}}, ObservedAt: time.Now().Unix()},
	}
}

// Pages own their rounds. A result that arrives after the person switched
// away updates the page that started the round — the same index on the now
// visible page is somebody else's account — and a hidden page starts none.
func TestPagesOwnTheirRounds(t *testing.T) {
	claude, claudeCh, claudeBegun := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com")
	codex, _, codexBegun := injectedPage(t, config.Codex, "x0@x.com", "x1@x.com")
	ui := &picker{pages: []*page{claude, codex}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()

	ui.show(ctx, 0)
	if *claudeBegun != 1 || *codexBegun != 0 {
		t.Fatalf("opening: claude began %d, codex began %d — only the visible page may start a round", *claudeBegun, *codexBegun)
	}

	// Switch away while Claude Code's round is still in flight…
	ui.show(ctx, 1)
	if *codexBegun != 1 {
		t.Fatalf("first visit to a page must start its round (began %d)", *codexBegun)
	}
	// …then its result lands.
	r := observed(61)
	r.Index = 1
	claudeCh <- r
	u, open := <-claude.updates
	claude.receive(u, open, time.Now())

	if got := claude.list[1].View.Obs; got == nil || got.Rows[0].Percent != 61 {
		t.Errorf("the originating page did not get its result: %+v", claude.list[1].View)
	}
	for i, d := range codex.list {
		if d.View.Obs != nil || d.View.Attempt.State != accountstate.AttemptPending {
			t.Errorf("codex[%d] was touched by Claude Code's round: %+v", i, d.View)
		}
	}
	if claude.updates == nil {
		t.Error("a round with its channel still open was closed out")
	}

	// The round closes on its own page, which schedules itself; the visible
	// page is unaffected.
	close(claudeCh)
	u, open = <-claude.updates
	claude.receive(u, open, time.Now())
	if claude.updates != nil || claude.nextAt.IsZero() {
		t.Errorf("round not closed out on its page: updates=%v nextAt=%v", claude.updates, claude.nextAt)
	}
	if codex.updates == nil {
		t.Error("closing Claude Code's round ended Codex's")
	}

	// Coming back does not start a second round on a page that has a list:
	// a deadline that passed while hidden is simply due at the next tick.
	ui.show(ctx, 0)
	if *claudeBegun != 1 {
		t.Errorf("returning to a page restarted it (%d rounds)", *claudeBegun)
	}
	claude.nextAt = time.Now().Add(-time.Second)
	if !claude.due(ui.lastKey) {
		t.Error("a page whose deadline passed while hidden must be due when visible")
	}
}

// A late or out-of-range result cannot index past its page's list.
func TestReceiveIgnoresAnIndexOutsideItsList(t *testing.T) {
	pg, _, _ := injectedPage(t, config.Codex, "only@x.com")
	pg.startRound(context.Background(), false)
	r := observed(9)
	r.Index = 5
	pg.receive(r, true, time.Now())
	if pg.list[0].View.Obs != nil {
		t.Error("a result addressed elsewhere landed on the only row")
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
