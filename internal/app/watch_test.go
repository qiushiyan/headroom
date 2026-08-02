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

func TestCarryOver(t *testing.T) {
	rows := []usage.Row{{Label: "5h session", Percent: 42}}
	old := []*accountData{
		{Acct: accounts.Account{Name: "resolved"},
			View: render.AccountView{Status: render.StatusRows, Rows: rows, Plan: "old plan"}},
		{Acct: accounts.Account{Name: "failed"},
			View: render.AccountView{Status: render.StatusFetchFailed}},
	}
	newList := []*accountData{
		{Acct: accounts.Account{Name: "resolved"},
			View: render.AccountView{Status: render.StatusPending, Plan: "new plan"}},
		{Acct: accounts.Account{Name: "failed"},
			View: render.AccountView{Status: render.StatusPending}},
		{Acct: accounts.Account{Name: "expired"},
			View: render.AccountView{Status: render.StatusExpired}},
		{Acct: accounts.Account{Name: "added"},
			View: render.AccountView{Status: render.StatusPending}},
	}
	carryOver(newList, old)

	// Resolved bars carry so the redraw doesn't blank; header fields stay fresh.
	if v := newList[0].View; v.Status != render.StatusRows || len(v.Rows) != 1 || v.Plan != "new plan" {
		t.Errorf("resolved: %+v", v)
	}
	// A previous failure does not carry — the new attempt shows as pending.
	if v := newList[1].View; v.Status != render.StatusPending {
		t.Errorf("failed: %+v", v)
	}
	// Non-pending states are this round's truth, never overwritten.
	if v := newList[2].View; v.Status != render.StatusExpired {
		t.Errorf("expired: %+v", v)
	}
	if v := newList[3].View; v.Status != render.StatusPending {
		t.Errorf("added: %+v", v)
	}
}

func TestSawRateLimit(t *testing.T) {
	ok := []*accountData{{View: render.AccountView{Status: render.StatusRows}}}
	limited := append(ok, &accountData{
		View: render.AccountView{Status: render.StatusHTTPError, HTTPCode: 429}})
	notLimited := append(ok, &accountData{
		View: render.AccountView{Status: render.StatusHTTPError, HTTPCode: 500}})
	if sawRateLimit(ok) || sawRateLimit(notLimited) || !sawRateLimit(limited) {
		t.Error("429 detection wrong")
	}
}
