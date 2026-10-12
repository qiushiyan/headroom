package placement

import (
	"strings"
	"testing"
	"time"
)

// A caller that can wait is refused rather than handed an account near a
// limit, told when the first of them has room again, and handed the same
// account as anyone else whenever one is open.
func TestACallerThatCanWaitIsRefusedNearALimit(t *testing.T) {
	near := acct("near", 85, 30) // 5h at 85%: back when the session window resets
	weekly := acct("weekly", 10, 92)
	weekly.Limits[1].ResetAt = at(30 * time.Hour) // all models at 92% until tomorrow
	both := acct("both", 90, 81)                  // back only once both have reset
	gone := acct("gone", 0, 0)
	gone.Excluded = "no headless token"

	d := chosen(t, []Candidate{near, weekly, both, gone}, Ledger{}, Intent{CanWait: true})
	if d.Chosen != "" {
		t.Fatalf("chose %q for a caller that can wait", d.Chosen)
	}
	if d.RoomAt != at(2*time.Hour) {
		t.Errorf("room at %d, want the 5h reset %d", d.RoomAt, at(2*time.Hour))
	}
	for _, want := range []string{"near (5h session 85%)", "weekly (All models (7d) 92%)", "both (5h session 90%)", "gone (no headless token)"} {
		if !strings.Contains(d.Refusal, want) {
			t.Errorf("refusal %q lacks %q", d.Refusal, want)
		}
	}

	// Without the flag the rule takes the least spent of them, as it always has.
	if d := chosen(t, []Candidate{near, weekly, both, gone}, Ledger{}, Intent{}); d.Chosen != "near" || d.Reason != ReasonNearLimit {
		t.Errorf("a launch: %q (%s)", d.Chosen, d.Reason)
	}

	// One open account and the flag changes nothing: the same choice, and an
	// owner near a limit is moved off as usual.
	open := acct("open", 40, 20)
	for _, intent := range []Intent{{}, {CanWait: true}} {
		if d := chosen(t, []Candidate{near, open}, Ledger{}, intent); d.Chosen != "open" || d.Reason != ReasonLeastLoad {
			t.Errorf("%+v: %q (%s)", intent, d.Chosen, d.Reason)
		}
	}
	if d := chosen(t, []Candidate{near, open}, Ledger{}, Intent{CanWait: true, Owner: "near"}); d.Chosen != "open" || d.Reason != ReasonMoved {
		t.Errorf("owner near a limit: %q (%s)", d.Chosen, d.Reason)
	}
}

// Room returns once every row at the threshold has reset; a row that names no
// reset, or whose percent did not parse, leaves its account's return unknown.
func TestWhenRoomReturns(t *testing.T) {
	both := acct("both", 90, 81)
	both.Limits[1].ResetAt = at(5 * time.Hour)
	noReset := acct("noreset", 95, 0)
	noReset.Limits[0].ResetAt = 0
	bad := acct("bad", 0, 0)
	bad.Limits[0].Bad = true

	cases := []struct {
		name  string
		cands []Candidate
		want  int64
	}{
		{"the later of an account's two resets", []Candidate{both}, at(5 * time.Hour)},
		{"no reset given", []Candidate{noReset}, 0},
		{"a percent that did not parse", []Candidate{bad}, 0},
		{"the first account back", []Candidate{noReset, both, acct("soon", 99, 0)}, at(2 * time.Hour)},
	}
	for _, tc := range cases {
		d := chosen(t, tc.cands, Ledger{}, Intent{CanWait: true})
		if d.Chosen != "" || d.RoomAt != tc.want {
			t.Errorf("%s: chose %q, room at %d, want %d", tc.name, d.Chosen, d.RoomAt, tc.want)
		}
	}

	// Nothing near a limit and nothing open: no account can take it at all,
	// and there is no instant to wait for.
	gone := acct("gone", 0, 0)
	gone.Excluded = "headless token ended"
	if d := chosen(t, []Candidate{gone}, Ledger{}, Intent{CanWait: true}); d.Chosen != "" || d.RoomAt != 0 || !strings.Contains(d.Refusal, "gone (headless token ended)") {
		t.Errorf("all excluded: %+v", d)
	}
}

// A placement whose session process is attached counts once: as the launch
// until the process is busy, then as the busy process, and as the launch again
// for the rest of its span once the process has gone.
func TestAnAttachedPlacementCountsOnce(t *testing.T) {
	var ledger Ledger
	ledger.RecordAs("p1", "uuid:a", "a", 0, "/h", now.Add(-time.Minute))
	a := acct("a", 0, 0)
	if c := chosen(t, []Candidate{a}, ledger, Intent{}).Candidates[0]; c.Pending != 1 || c.Load != 1 {
		t.Fatalf("placed: pending %d load %d", c.Pending, c.Load)
	}
	if _, ok := ledger.Attach("nope", 7); ok {
		t.Error("attached an id nobody placed")
	}
	if p, ok := ledger.Attach("p1", 4242); !ok || p.Key != "uuid:a" || p.PID != 4242 {
		t.Fatalf("attach: %+v %v", p, ok)
	}
	a.Busy = []Proc{{PID: 4242, StartedMS: now.Add(-50 * time.Second).UnixMilli(), Home: "/h"}}
	if c := chosen(t, []Candidate{a}, ledger, Intent{}).Candidates[0]; c.Busy != 1 || c.Pending != 0 || c.Load != 1 {
		t.Errorf("running: busy %d pending %d load %d", c.Busy, c.Pending, c.Load)
	}
	a.Busy = nil
	if c := chosen(t, []Candidate{a}, ledger, Intent{}).Candidates[0]; c.Pending != 1 || c.Load != 1 {
		t.Errorf("exited: pending %d load %d", c.Pending, c.Load)
	}
}
