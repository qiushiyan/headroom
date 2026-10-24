package app

import (
	"context"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/refresh"
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
	// Rows with something to ask about, as prepare leaves a usable account.
	asking := func(keys ...state.Key) []*accountData {
		list := make([]*accountData, len(keys))
		for i, k := range keys {
			list[i] = &accountData{Key: k, Request: &refresh.Candidate{}}
		}
		return list
	}
	ui := &page{st: st, list: asking(keys...)}
	ui.schedule()

	if wait := ui.nextAt.Sub(now); wait < config.DefaultSpacing-2*time.Second {
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
	ui = &page{st: st, list: asking(keys...)}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait > config.DefaultSpacing/2+2*time.Second {
		t.Errorf("next round in %v; the sooner account was ignored", wait)
	}

	// And an empty ledger — every claim failed, so nothing recorded an
	// eligibility — must not become a busy loop.
	ui = &page{st: state.Open(config.Scope{AccountsRoot: t.TempDir()}), list: asking(keys[0])}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait < refreshFloor-2*time.Second {
		t.Errorf("next round in %v; the floor must hold when nothing is scheduled", wait)
	}

	// A board with nothing to ask about must not treat the floor as a cadence
	// either — each round costs a `claude auth status` spawn per account to
	// re-learn the same answer — and backs off to the request spacing, the
	// slowest rate at which it could tell the user anything new. That it
	// stays under the presence window is TestAnIdleBoardStillComesBack's
	// business.
	ui = &page{st: state.Open(config.Scope{AccountsRoot: t.TempDir()}), list: []*accountData{{Key: keys[0]}}}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait < config.DefaultSpacing-2*time.Second {
		t.Errorf("idle board polls every %v, at a subprocess spawn per account; want the request spacing", wait)
	}
}

// An unattended board stops asking. Presence is what watch got for free from
// the user having chosen to run a long-lived command, and what a picker left
// open on a spare tab does not.
func TestPickerPausesWhenNobodyIsThere(t *testing.T) {
	now := time.Now()
	lastKey := now.Add(-presenceWindow - time.Minute)
	ui := &page{nextAt: now.Add(-time.Second)}
	if ui.due(lastKey) {
		t.Error("an unattended board kept polling")
	}
	// A refresh asked for by hand fires regardless of presence: it was asked
	// for. It waits only for the floor, never for the cadence.
	ui.armed, ui.lastLocal = true, now.Add(-refreshFloor-time.Second)
	if !ui.due(lastKey) {
		t.Error("an armed refresh must fire even after the board paused")
	}
	ui.lastLocal = now
	if ui.due(lastKey) {
		t.Error("an armed refresh inside the floor must wait it out")
	}
	// And a keypress resumes the ordinary cadence.
	ui.armed, lastKey = false, now
	if !ui.due(lastKey) {
		t.Error("a present user must get the ordinary cadence back")
	}
	// A round already in flight never starts a second one, armed or not.
	ui.updates = make(chan refresh.Result)
	ui.armed = true
	if ui.due(lastKey) {
		t.Error("rounds must not overlap")
	}
}

// `r` means now. The claim — not this loop — is what decides per account
// whether a request leaves, so the only things that defer the round are a
// round already in flight and the subprocess-spawn floor; both remember the
// press instead of dropping it.
func TestRefreshRunsNowAndOnlyTheFloorDefers(t *testing.T) {
	ctx := context.Background()
	ui, _, begun := injectedPage(t, config.Claude, "a@x.com")
	ui.startRound(ctx, false) // its account is waiting on the budget
	ui.updates = nil          // and the round has drained

	// Past the floor `r` runs a round immediately — even mid-quiet-period,
	// even with accounts waiting on the budget: eligibility is the claim's
	// question, and pre-deciding it here is the duplicated-eligibility bug
	// the store's locked test-and-set exists to close.
	ui.lastLocal, ui.nextAt = time.Now().Add(-refreshFloor-time.Second), time.Now().Add(time.Minute)
	ui.refresh(ctx)
	if *begun != 2 || ui.updates == nil || ui.armed || !ui.manual {
		t.Fatalf("r mid-quiet-period did not run a round of its own (rounds %d, armed %v); only the claim may defer a request", *begun, ui.armed)
	}

	// Inside the floor the press arms — spawn hygiene for prepare's
	// per-account `claude auth status`, not budget politeness.
	ui.updates = nil
	ui.refresh(ctx)
	if !ui.armed || ui.updates != nil || *begun != 2 {
		t.Errorf("r inside the floor: armed %v, rounds %d — it must arm and start nothing", ui.armed, *begun)
	}

	// During a round it arms too, floor or not: the round in flight only
	// carries the accounts that were eligible when it started.
	ui.armed, ui.lastLocal = false, time.Now().Add(-refreshFloor-time.Second)
	ui.updates = make(chan refresh.Result)
	ui.refresh(ctx)
	if !ui.armed || *begun != 2 {
		t.Errorf("r during a round: armed %v, rounds %d — it must arm and start nothing", ui.armed, *begun)
	}

	// A held key delivers dozens of presses; all coalesce into the one armed
	// bit, and none spawns anything.
	for range 50 {
		ui.refresh(ctx)
	}
	if !ui.armed || *begun != 2 {
		t.Errorf("a burst of r: armed %v, rounds %d", ui.armed, *begun)
	}
}

// A board with nothing to fetch — every token aged out, say — still has to
// come back, and the round it comes back with must not be schedulable past
// the presence window that gates it.
func TestAnIdleBoardStillComesBack(t *testing.T) {
	now := time.Now()
	ui := &page{st: state.Open(config.Scope{AccountsRoot: t.TempDir()}), list: []*accountData{{Key: state.Key{Name: "a"}}}}
	ui.schedule()

	if wait := ui.nextAt.Sub(now); wait >= presenceWindow {
		t.Errorf("next round in %v, but presence expires at %v — measured from the same "+
			"instant, so the round can never fire", wait, presenceWindow)
	}
}
