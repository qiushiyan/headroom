package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

// The board's cadence is eligibility itself. The trap: a floor written as a
// starting value silently becomes a ceiling, and the board then polls every
// 30 seconds against a budget that refuses anything under 90 — every third
// round doing nothing but spending the claim.
func TestPickerSchedulesAtTheNextEligibleInstant(t *testing.T) {
	now := time.Now()
	keys := []state.Key{{Name: "a"}, {Name: "b"}}

	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	if _, err := st.Claim(keys, now); err != nil {
		t.Fatal(err)
	}
	accts := func(keys ...state.Key) []accountstate.Account {
		out := make([]accountstate.Account, len(keys))
		for i, k := range keys {
			out[i] = accountstate.Account{Key: k}
		}
		return out
	}
	// A round with something to ask about, as prepare leaves a usable account.
	next := func(st *state.Store, asking bool, keys ...state.Key) time.Time {
		return nextRound(st.Load(), accts(keys...), asking, config.DefaultSpacing, now)
	}

	if wait := next(st, true, keys...).Sub(now); wait < config.DefaultSpacing-2*time.Second {
		t.Errorf("next round in %v; nothing new is obtainable before %v", wait, config.DefaultSpacing)
	}

	// One account eligible sooner than the other sets the pace: the board is a
	// comparison surface, so it refreshes as soon as any row could change.
	st = state.Open(config.Scope{AccountsRoot: t.TempDir()})
	if _, err := st.Claim(keys[:1], now.Add(-config.DefaultSpacing/2)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(keys[1:], now); err != nil {
		t.Fatal(err)
	}
	if wait := next(st, true, keys...).Sub(now); wait > config.DefaultSpacing/2+2*time.Second {
		t.Errorf("next round in %v; the sooner account was ignored", wait)
	}

	// And an empty ledger — every claim failed, so nothing recorded an
	// eligibility — must not become a busy loop.
	if wait := next(state.Open(config.Scope{AccountsRoot: t.TempDir()}), true, keys[0]).Sub(now); wait < refreshFloor-2*time.Second {
		t.Errorf("next round in %v; the floor must hold when nothing is scheduled", wait)
	}
	// Nor may an account coming eligible inside the floor: accounts claimed a
	// few seconds apart would each start a round, and every round spawns a
	// health probe per account. Deleting `|| next.Before(floor)` fails here.
	st = state.Open(config.Scope{AccountsRoot: t.TempDir()})
	if _, err := st.Claim(keys[:1], now.Add(-config.DefaultSpacing+5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if wait := next(st, true, keys[0]).Sub(now); wait < refreshFloor-2*time.Second {
		t.Errorf("next round in %v for an account eligible in 5s; the floor must hold", wait)
	}

	// A board with nothing to ask about must not treat the floor as a cadence
	// either — each round costs a `claude auth status` spawn per account to
	// re-learn the same answer — and backs off to the request spacing, the
	// slowest rate at which it could tell the user anything new. That it
	// stays under the presence window is TestAnIdleBoardStillComesBack's
	// business.
	if wait := next(state.Open(config.Scope{AccountsRoot: t.TempDir()}), false, keys[0]).Sub(now); wait < config.DefaultSpacing-2*time.Second {
		t.Errorf("idle board polls every %v, at a subprocess spawn per account; want the request spacing", wait)
	}
}

// fakeClock is a clock a test holds: a page reads every instant of its
// schedule from it, so a test moves time instead of the schedule's fields.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock               { return &fakeClock{t: time.Now()} }
func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// clockedPage is an injected page on a clock the test holds.
func clockedPage(t *testing.T, names ...string) (*page, *fakeClock, *int) {
	t.Helper()
	pg, _, begun := injectedPage(t, config.Claude, names...)
	clock := newFakeClock()
	pg.now = clock.Now
	return pg, clock, begun
}

// closeRound is the page's round ending: its channel closed and drained.
func closeRound(pg *page) { pg.receive(round.Result{}, false) }

// refreshIn is how long the page says its next round is away.
func refreshIn(t *testing.T, pg *page, lastKey time.Time) time.Duration {
	t.Helper()
	said := pg.refreshStatus(lastKey)
	d, err := time.ParseDuration(strings.TrimPrefix(said, "next refresh in "))
	if err != nil {
		t.Fatalf("the page does not say when its next round is: %q", said)
	}
	return d
}

// An unattended board stops asking. Presence is what watch got for free from
// the user having chosen to run a long-lived command, and what a picker left
// open on a spare tab does not.
func TestPickerPausesWhenNobodyIsThere(t *testing.T) {
	ctx := context.Background()
	pg, clock, begun := clockedPage(t, "a@x.com")
	lastKey := clock.Now() // the last key anybody pressed
	pg.startRound(ctx, false)
	closeRound(pg)

	clock.advance(presenceWindow + time.Minute) // long past due, and nobody there
	pg.tick(ctx, lastKey)
	if *begun != 1 || pg.refreshStatus(lastKey) != "paused — r to refresh" {
		t.Errorf("an unattended board kept polling: %d rounds, %q", *begun, pg.refreshStatus(lastKey))
	}

	// A refresh asked for by hand fires regardless of presence: it was asked
	// for. It waits only for the floor, never for the cadence. Pressed while
	// a round is in flight it is queued; that round closes inside the floor.
	pg.refresh(ctx)
	pg.refresh(ctx)
	closeRound(pg)
	if *begun != 2 || !strings.HasPrefix(pg.refreshStatus(lastKey), "queued · refresh in ") {
		t.Fatalf("a press during a round: %d rounds, %q", *begun, pg.refreshStatus(lastKey))
	}
	pg.tick(ctx, lastKey)
	if *begun != 2 {
		t.Error("an armed refresh inside the floor must wait it out")
	}
	clock.advance(refreshFloor + time.Second)
	pg.tick(ctx, lastKey)
	if *begun != 3 {
		t.Error("an armed refresh must fire even after the board paused")
	}
	closeRound(pg)

	// And a keypress resumes the ordinary cadence. (The round was asked for
	// by hand, so its answer holds the footer first.)
	clock.advance(ackTTL)
	clock.advance(refreshIn(t, pg, clock.Now()) + time.Second)
	pg.tick(ctx, lastKey)
	if *begun != 3 {
		t.Error("the cadence resumed with nobody there")
	}
	pg.tick(ctx, clock.Now())
	if *begun != 4 {
		t.Error("a present user must get the ordinary cadence back")
	}

	// A round already in flight never starts a second one, armed or not.
	pg.tick(ctx, clock.Now())
	pg.refresh(ctx)
	clock.advance(time.Hour)
	pg.tick(ctx, clock.Now())
	if *begun != 4 {
		t.Errorf("rounds must not overlap: %d", *begun)
	}
}

// `r` means now. The claim — not this loop — is what decides per account
// whether a request leaves, so the only things that defer the round are a
// round already in flight and the subprocess-spawn floor; both remember the
// press instead of dropping it.
func TestRefreshRunsNowAndOnlyTheFloorDefers(t *testing.T) {
	ctx := context.Background()
	pg, clock, begun := clockedPage(t, "a@x.com")
	pg.startRound(ctx, false) // its account is waiting on the budget
	closeRound(pg)            // and the round has drained

	// Past the floor `r` runs a round immediately — even mid-quiet-period,
	// even with accounts waiting on the budget: eligibility is the claim's
	// question, and pre-deciding it here is the duplicated-eligibility bug
	// the store's locked test-and-set exists to close.
	clock.advance(refreshFloor + time.Second)
	if refreshIn(t, pg, clock.Now()) <= 0 {
		t.Fatal("the fixture's next round is already due; the press would prove nothing")
	}
	pg.refresh(ctx)
	if *begun != 2 || pg.refreshStatus(clock.Now()) != "refreshing…" {
		t.Fatalf("r mid-quiet-period did not run a round of its own (rounds %d, %q); only the claim may defer a request", *begun, pg.refreshStatus(clock.Now()))
	}
	// The round a person asked for answers them when it lands.
	closeRound(pg)
	if said := pg.refreshStatus(clock.Now()); !strings.HasPrefix(said, "refreshed · ") {
		t.Errorf("a round asked for by hand was not acknowledged: %q", said)
	}

	// Inside the floor the press arms — spawn hygiene for prepare's
	// per-account `claude auth status`, not budget politeness.
	pg.refresh(ctx)
	if *begun != 2 || !strings.HasPrefix(pg.refreshStatus(clock.Now()), "queued · refresh in ") {
		t.Errorf("r inside the floor: rounds %d, %q — it must arm and start nothing", *begun, pg.refreshStatus(clock.Now()))
	}
	// The queued round fires the moment the floor passes.
	clock.advance(refreshFloor + time.Second)
	pg.tick(ctx, clock.Now())
	if *begun != 3 {
		t.Fatalf("the queued round did not fire once the floor passed: %d rounds", *begun)
	}

	// During a round it arms too, floor or not: the round in flight only
	// carries the accounts that were eligible when it started.
	clock.advance(refreshFloor + time.Second)
	pg.refresh(ctx)
	if *begun != 3 || pg.refreshStatus(clock.Now()) != "refreshing… · queued" {
		t.Errorf("r during a round: rounds %d, %q — it must arm and start nothing", *begun, pg.refreshStatus(clock.Now()))
	}

	// A held key delivers dozens of presses; all coalesce into the one queued
	// round, and none spawns anything.
	for range 50 {
		pg.refresh(ctx)
	}
	if *begun != 3 {
		t.Errorf("a burst of r: rounds %d", *begun)
	}
	closeRound(pg)
	pg.tick(ctx, clock.Now())
	if *begun != 4 {
		t.Errorf("the burst left %d rounds, want the one it queued", *begun)
	}
}

// A board with nothing to fetch — every token aged out, say — still has to
// come back, and the round it comes back with must not be schedulable past
// the presence window that gates it.
func TestAnIdleBoardStillComesBack(t *testing.T) {
	ctx := context.Background()
	pg, clock, begun := clockedPage(t, "a") // an injected account has no request: nothing to ask about
	lastKey := clock.Now()
	pg.startRound(ctx, false)
	closeRound(pg)

	if wait := refreshIn(t, pg, lastKey); wait >= presenceWindow {
		t.Errorf("next round in %v, but presence expires at %v — measured from the same "+
			"instant, so the round can never fire", wait, presenceWindow)
	}
	clock.advance(presenceWindow - time.Second)
	pg.tick(ctx, lastKey)
	if *begun != 2 {
		t.Error("the idle board never came back inside the window its last key opened")
	}
}
