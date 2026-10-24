package round

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

// codexFixture is a fixture home with a Codex primary and extras, and a local
// server standing in for the usage endpoint. The server proves the request
// and its interpretation — never the vendor's behaviour.
type codexFixture struct {
	cfg   config.Config
	scope config.Scope
}

func newCodexFixture(t *testing.T, usageURL string) codexFixture {
	t.Helper()
	cfg := config.ForHome(t.TempDir())
	cfg.Claude.PrimaryName = "primary"
	cfg.Codex.Present = true
	cfg.Codex.UsageURL = usageURL
	if err := os.MkdirAll(cfg.Codex.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return codexFixture{cfg: cfg, scope: cfg.Codex}
}

// extra seeds one extra Codex home the way `accounts add` would.
func (f codexFixture) extra(t *testing.T, l codexauthtest.Login) string {
	t.Helper()
	dir := filepath.Join(f.scope.AccountsRoot, l.Email)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.scope.StoreDir(), filepath.Join(dir, f.scope.StoreLink())); err != nil {
		t.Fatal(err)
	}
	l.Write(t, dir)
	return dir
}

func login(n int) codexauthtest.Login {
	return codexauthtest.Login{
		Email: fmt.Sprintf("u%d@x.com", n), Plan: "pro",
		AccountID: fmt.Sprintf("acct-%d", n), UserID: fmt.Sprintf("user-%d", n), Token: fmt.Sprintf("tok-%d", n),
	}
}

func codexUsage(accountID, userID string, percent int) string {
	return fmt.Sprintf(`{"plan_type":"prolite","account_id":%q,"user_id":%q,
	  "rate_limit":{"allowed":true,"limit_reached":false,
	    "primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_after_seconds":1000,"reset_at":%d},
	    "secondary_window":null},
	  "spend_control":{"reached":false},"rate_limit_reached_type":null}`,
		accountID, userID, percent, time.Now().Add(72*time.Hour).Unix())
}

// Identity travels with the observation. A login that changes after discovery
// changes neither the row's label nor whose response lands on it within that
// round, and a body naming someone else is never shown.
func TestCodexIdentityWithinARound(t *testing.T) {
	var authSeen, acctSeen string
	reply := codexUsage("acct-1", "user-1", 55)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen, acctSeen = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
		w.Write([]byte(reply))
	}))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	first, second := login(1), login(2)
	dir := f.extra(t, first)

	// The home is logged into another account right after discovery — before
	// preparation, which must work from the snapshot and never read the file
	// again: a re-read there would send the second login's token under the
	// first's label and ledger key.
	st := state.Open(f.scope)
	set := accounts.Discover(f.scope)
	second.Write(t, dir)
	r := prepare(context.Background(), set, st, time.Now(), Deps{})
	var d *row
	for u := range r.Fetch(context.Background()) {
		r.Apply(u)
		d = r.rows[u.Index]
	}
	if d == nil {
		t.Fatal("no request left")
	}
	if authSeen != "Bearer "+first.AccessToken() || acctSeen != "acct-1" {
		t.Errorf("the request mixed logins: %q / %q", authSeen, acctSeen)
	}
	if d.Acct.Email != "u1@x.com" || d.View.Obs == nil || d.View.Obs.Rows[0].Percent != 55 {
		t.Errorf("row after a mid-round re-login: %+v obs %+v", d.Acct.Email, d.View.Obs)
	}
	// Next round the home is the second login: a different key, so the first
	// login's stored response is not replayed under it.
	reply = codexUsage("acct-1", "user-1", 99) // and the endpoint answers for someone else
	time.Sleep(time.Millisecond)
	again := Prepare(context.Background(), f.scope, state.Open(f.scope), time.Now(), Deps{})
	again.Complete(context.Background())
	v := byName(again)["u1@x.com"].View
	if v.Attempt.State != accountstate.AttemptUnparseable || v.Obs != nil {
		t.Errorf("a response naming another account was shown: attempt %+v obs %+v", v.Attempt, v.Obs)
	}
}

// The health and eligibility table, through the access reader. "Relogin
// required" is never produced for Codex.
func TestCodexAccessTable(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1")
	mk := func(name string, doc []byte) {
		dir := filepath.Join(f.scope.AccountsRoot, name)
		if doc == nil {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return
		}
		codexauthtest.WriteRaw(t, dir, doc)
	}
	stale := login(3)
	stale.Expires = time.Now().Add(-time.Hour)
	mk("absent@x.com", nil)
	mk("garbage@x.com", []byte(`not json`))
	mk("apikey@x.com", []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`))
	mk("notokens@x.com", []byte(`{"auth_mode":"chatgpt","tokens":{}}`))
	mk("noident@x.com", codexauthtest.Login{Email: "noident@x.com", AccountID: "acct-7", Token: "t"}.Document())
	mk("stale@x.com", stale.Document())
	mk("good@x.com", login(4).Document())

	set := accounts.Discover(f.scope)
	got := byName(prepare(context.Background(), set, state.Open(f.scope), time.Now(), Deps{}))
	for name, want := range map[string]struct {
		health  accountstate.Health
		attempt accountstate.AttemptState
		request bool
	}{
		"absent@x.com":   {accountstate.HealthNoLogin, accountstate.AttemptNone, false},
		"garbage@x.com":  {accountstate.HealthBadBlob, accountstate.AttemptNone, false},
		"apikey@x.com":   {accountstate.HealthUnknown, accountstate.AttemptNone, false},
		"notokens@x.com": {accountstate.HealthBadBlob, accountstate.AttemptNone, false},
		"noident@x.com":  {accountstate.HealthOK, accountstate.AttemptIdentityUnknown, false},
		"stale@x.com":    {accountstate.HealthOK, accountstate.AttemptTokenStale, false},
		"good@x.com":     {accountstate.HealthOK, accountstate.AttemptPending, true},
	} {
		d := got[name]
		if d == nil {
			t.Fatalf("%s not discovered", name)
		}
		if d.View.Health != want.health || d.View.Attempt.State != want.attempt || (d.request != nil) != want.request {
			t.Errorf("%s: health %v attempt %v request %v", name, d.View.Health, d.View.Attempt.State, d.request != nil)
		}
		if d.View.Health == accountstate.HealthReloginRequired {
			t.Errorf("%s: relogin required is never produced for Codex", name)
		}
	}
	if got["apikey@x.com"].View.AuthMode != "apikey" {
		t.Errorf("the caption cannot name the auth mode: %+v", got["apikey@x.com"].View)
	}
	if got["good@x.com"].View.Plan != "pro" {
		t.Errorf("plan before any response = %q, want the auth snapshot's", got["good@x.com"].View.Plan)
	}
}

// Start refuses a candidate of the other vendor before any claim.
func TestStartRefusesTheOtherVendorsCandidate(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	f.extra(t, login(1))
	set := accounts.Discover(f.scope)
	var reqs []*refresh.Candidate
	for _, d := range prepare(context.Background(), set, state.Open(f.scope), time.Now(), Deps{}).rows {
		reqs = append(reqs, d.request)
	}
	claudeStore := state.Open(f.cfg.Claude)
	for r := range refresh.Start(context.Background(), claudeStore, reqs, nil) {
		if r.Attempt.State != accountstate.AttemptStateUnavailable || r.StoreErr == nil {
			t.Errorf("a Codex candidate was accepted by the Claude Code store: %+v", r)
		}
	}
	if hits != 0 {
		t.Errorf("%d request(s) left through the wrong store", hits)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Claude.AccountsRoot, "state.json")); err == nil {
		t.Error("a claim was written to Claude Code's ledger")
	}
}
