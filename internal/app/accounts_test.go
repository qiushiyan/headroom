package app

import (
	"context"
	"testing"
	"time"

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

	// But a board with nothing to ask about — every account logged out, or its
	// token aged out — must not treat the floor as a cadence. Each round costs
	// a `claude auth status` per account to re-learn the same answer.
	ui = &picker{st: state.Open(t.TempDir()), list: []*accountData{{Key: keys[0]}}}
	ui.schedule()
	if wait := ui.nextAt.Sub(now); wait < presenceWindow-2*time.Second {
		t.Errorf("next round in %v; nothing was asking, so there is nothing to wait for", wait)
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
	// A refresh asked for by hand fires regardless: it was asked for.
	ui.armed = true
	if !ui.due() {
		t.Error("an armed refresh must fire even after the board paused")
	}
	// And a keypress resumes the ordinary cadence.
	ui.armed, ui.lastKey = false, now
	if !ui.due() {
		t.Error("a present user must get the ordinary cadence back")
	}
	// A round already in flight never starts a second one.
	ui.updates = make(chan fetchUpdate)
	if ui.due() {
		t.Error("rounds must not overlap")
	}
}

// `r` is never a no-op. Inside a quiet period it arms the next round, and that
// has to hold while a round is in flight too: the round already running only
// carries the accounts that were eligible when it started.
func TestRefreshIsRememberedNotDropped(t *testing.T) {
	now := time.Now()
	st := state.Open(t.TempDir())
	ui := &picker{st: st, nextAt: now.Add(time.Minute), lastKey: now}

	ui.refresh(context.Background())
	if !ui.armed {
		t.Error("r inside a quiet period was dropped instead of arming the next round")
	}

	ui.armed = false
	ui.updates = make(chan fetchUpdate)
	ui.refresh(context.Background())
	if !ui.armed {
		t.Error("r during a round was dropped; the round in flight carries only the "+
			"accounts that were eligible when it started")
	}
}
