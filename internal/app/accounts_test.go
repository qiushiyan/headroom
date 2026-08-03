package app

import (
	"context"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// The board's cadence is eligibility itself. The trap: a floor written as a
// starting value silently becomes a ceiling, and the board then polls every
// 30 seconds against a budget that refuses anything under 90 — every third
// round doing nothing but spending the claim.
func TestPickerSchedulesAtTheNextEligibleInstant(t *testing.T) {
	now := time.Now()
	keys := []state.Key{{Name: "a"}, {Name: "b"}}

	st := state.Open(t.TempDir())
	if _, err := st.Claim(keys, now); err != nil {
		t.Fatal(err)
	}
	ui := &picker{st: st, wanted: 2, list: []*accountData{{Key: keys[0]}, {Key: keys[1]}}}
	ui.schedule()

	if wait := ui.nextAt.Sub(now); wait < usage.RequestSpacing-2*time.Second {
		t.Errorf("next round in %v; nothing new is obtainable before %v", wait, usage.RequestSpacing)
	}

	// One account eligible sooner than the other sets the pace: the board is a
	// comparison surface, so it refreshes as soon as any row could change.
	st = state.Open(t.TempDir())
	if _, err := st.Claim(keys[:1], now.Add(-usage.RequestSpacing/2)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(keys[1:], now); err != nil {
		t.Fatal(err)
	}
	ui = &picker{st: st, wanted: 2, list: []*accountData{{Key: keys[0]}, {Key: keys[1]}}}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait > usage.RequestSpacing/2+2*time.Second {
		t.Errorf("next round in %v; the sooner account was ignored", wait)
	}

	// And an empty ledger — every claim failed, so nothing recorded an
	// eligibility — must not become a busy loop.
	ui = &picker{st: state.Open(t.TempDir()), wanted: 1, list: []*accountData{{Key: keys[0]}}}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait < refreshFloor-2*time.Second {
		t.Errorf("next round in %v; the floor must hold when nothing is scheduled", wait)
	}

	// A board with nothing to ask about must not treat the floor as a cadence
	// either — each round costs a `claude auth status` spawn per account to
	// re-learn the same answer. How far it backs off is
	// TestAnIdleBoardStillComesBack's business.
	ui = &picker{st: state.Open(t.TempDir()), list: []*accountData{{Key: keys[0]}}}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait <= refreshFloor {
		t.Errorf("idle board polls every %v, at a subprocess spawn per account", wait)
	}
}

// An unattended board stops asking. Presence is what watch got for free from
// the user having chosen to run a long-lived command, and what a picker left
// open on a spare tab does not.
func TestPickerPausesWhenNobodyIsThere(t *testing.T) {
	now := time.Now()
	ui := &picker{nextAt: now.Add(-time.Second), lastKey: now.Add(-presenceWindow - time.Minute)}
	if ui.due() {
		t.Error("an unattended board kept polling")
	}
	// A refresh asked for by hand fires regardless of presence: it was asked
	// for. It waits only for the floor, never for the cadence.
	ui.armed, ui.lastLocal = true, now.Add(-refreshFloor-time.Second)
	if !ui.due() {
		t.Error("an armed refresh must fire even after the board paused")
	}
	ui.lastLocal = now
	if ui.due() {
		t.Error("an armed refresh inside the floor must wait it out")
	}
	// And a keypress resumes the ordinary cadence.
	ui.armed, ui.lastKey = false, now
	if !ui.due() {
		t.Error("a present user must get the ordinary cadence back")
	}
	// A round already in flight never starts a second one, armed or not.
	ui.updates = make(chan fetchUpdate)
	ui.armed = true
	if ui.due() {
		t.Error("rounds must not overlap")
	}
}

// `r` means now. The claim — not this loop — is what decides per account
// whether a request leaves, so the only things that defer the round are a
// round already in flight and the subprocess-spawn floor; both remember the
// press instead of dropping it.
func TestRefreshRunsNowAndOnlyTheFloorDefers(t *testing.T) {
	now := time.Now()
	ui := &picker{st: state.Open(t.TempDir())}

	// Past the floor `r` runs immediately — even mid-quiet-period, even with
	// accounts waiting on the budget (wanted > 0): eligibility is the claim's
	// question, and pre-deciding it here is the duplicated-eligibility bug
	// the store's locked test-and-set exists to close.
	ui.wanted, ui.lastLocal, ui.nextAt = 1, now.Add(-refreshFloor-time.Second), now.Add(time.Minute)
	if !ui.refreshNow(now) {
		t.Error("r mid-quiet-period was deferred; only the claim may defer a request")
	}

	// Inside the floor the press arms — spawn hygiene for prepare's
	// per-account `claude auth status`, not budget politeness.
	ui.lastLocal = now
	ui.refresh(context.Background())
	if !ui.armed {
		t.Error("r inside the floor was dropped instead of arming")
	}
	if ui.updates != nil {
		t.Error("r inside the floor must not start a round")
	}

	// During a round it arms too: the round in flight only carries the
	// accounts that were eligible when it started.
	ui.armed = false
	ui.updates = make(chan fetchUpdate)
	ui.refresh(context.Background())
	if !ui.armed {
		t.Error("r during a round was dropped")
	}

	// A held key delivers dozens of presses; all coalesce into the one armed
	// bit, and none spawns anything.
	for i := 0; i < 50; i++ {
		ui.refresh(context.Background())
	}
	if !ui.armed {
		t.Error("a burst of r lost the armed bit")
	}
}

// A stale round's update must never land on a rebuilt list: idx addresses
// positions in the list its own round was built over, and rediscovery between
// rounds can shrink or reorder that list under an in-flight goroutine.
func TestStaleRoundUpdatesAreDropped(t *testing.T) {
	ui := &picker{round: 2, list: []*accountData{{}}}
	// idx 5 panics against a len-1 list unless the stamp drops it first.
	ui.apply(fetchUpdate{idx: 5, round: 1}, time.Now())

	// A current-round update still lands.
	ui.apply(fetchUpdate{idx: 0, round: 2, out: fetchOutcome{attempt: render.AttemptTransport}}, time.Now())
	if ui.list[0].View.Attempt.State != render.AttemptTransport {
		t.Error("a current-round update was not applied")
	}
}

// A board with nothing to fetch — every token aged out, say — still has to
// come back, and the round it comes back with must not be schedulable past
// the presence window that gates it.
func TestAnIdleBoardStillComesBack(t *testing.T) {
	now := time.Now()
	ui := &picker{st: state.Open(t.TempDir()), list: []*accountData{{Key: state.Key{Name: "a"}}}}
	ui.schedule()

	if wait := ui.nextAt.Sub(now); wait >= presenceWindow {
		t.Errorf("next round in %v, but presence expires at %v — measured from the same "+
			"instant, so the round can never fire", wait, presenceWindow)
	}
}
