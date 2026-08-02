package app

import (
	"io"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/usage"
)

func TestWatchInterval(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want time.Duration
		ok   bool
	}{
		{"default", nil, defaultInterval, true},
		{"explicit", []string{"--interval", "90s"}, 90 * time.Second, true},
		{"equals form", []string{"--interval=5m"}, 5 * time.Minute, true},
		{"at the floor", []string{"--interval", "30s"}, 30 * time.Second, true},
		{"below the floor", []string{"--interval", "5s"}, 0, false},
		{"garbage duration", []string{"--interval", "soon"}, 0, false},
		{"missing value", []string{"--interval"}, 0, false},
		{"unknown flag", []string{"--fast"}, 0, false},
	}
	for _, c := range cases {
		got, ok := watchInterval(c.args, io.Discard)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: watchInterval(%v) = %v, %v; want %v, %v",
				c.name, c.args, got, ok, c.want, c.ok)
		}
	}
}

// carryOver used to assert that a previous round's rows become ordinary
// current rows. That assertion was the bug in test form: it committed the
// design to presenting old numbers as if they had just been observed. The
// contract now is that an observation carries *whole* — its timestamp and
// source travel with it, so age survives the round boundary.
func TestCarryOverPreservesProvenance(t *testing.T) {
	observedAt := time.Now().Add(-40 * time.Second).Unix()
	old := []*accountData{
		{Acct: accounts.Account{Name: "resolved"}, View: render.AccountView{
			Plan: "old plan",
			Obs: &render.Observation{
				Rows:       []usage.Row{{Label: "5h session", Percent: 42}},
				ObservedAt: observedAt,
				Source:     render.SourceLive,
			}}},
		{Acct: accounts.Account{Name: "never-observed"}, View: render.AccountView{
			Attempt: render.Attempt{State: render.AttemptTransport}}},
	}
	newList := []*accountData{
		{Acct: accounts.Account{Name: "resolved"}, View: render.AccountView{
			Plan: "new plan", Attempt: render.Attempt{State: render.AttemptPending}}},
		{Acct: accounts.Account{Name: "never-observed"}, View: render.AccountView{
			Attempt: render.Attempt{State: render.AttemptPending}}},
		{Acct: accounts.Account{Name: "added"}, View: render.AccountView{
			Attempt: render.Attempt{State: render.AttemptPending}}},
	}
	carryOver(newList, old)

	v := newList[0].View
	if v.Obs == nil || len(v.Obs.Rows) != 1 {
		t.Fatalf("rows did not carry: %+v", v.Obs)
	}
	if v.Obs.ObservedAt != observedAt {
		t.Errorf("carried rows were restamped as fresh: got %d want %d", v.Obs.ObservedAt, observedAt)
	}
	if v.Plan != "new plan" {
		t.Errorf("header fields must be this round's: plan=%q", v.Plan)
	}
	// Nothing was ever observed for these two — there is nothing to carry.
	if newList[1].View.Obs != nil || newList[2].View.Obs != nil {
		t.Error("invented an observation for an account that never had one")
	}
}

// A newer observation already loaded this round (a live fetch that landed, or
// a fresher Claude Code cache) must not be overwritten by an older carried one.
func TestCarryOverKeepsTheNewerObservation(t *testing.T) {
	now := time.Now().Unix()
	old := []*accountData{{Acct: accounts.Account{Name: "a"}, View: render.AccountView{
		Obs: &render.Observation{ObservedAt: now - 300, Source: render.SourceLive,
			Rows: []usage.Row{{Label: "old"}}}}}}
	newList := []*accountData{{Acct: accounts.Account{Name: "a"}, View: render.AccountView{
		Obs: &render.Observation{ObservedAt: now - 10, Source: render.SourceCache,
			Rows: []usage.Row{{Label: "new"}}}}}}
	carryOver(newList, old)
	if newList[0].View.Obs.Rows[0].Label != "new" {
		t.Error("a stale carried observation displaced a newer one")
	}
}
