package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
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

// round runs one prepare → claim → fetch → resolve round, as the board does.
func (f codexFixture) round(t *testing.T) map[string]accountstate.Account {
	t.Helper()
	r := round.Prepare(context.Background(), f.scope, state.Open(f.scope), time.Now(), LiveRound(true))
	r.Complete(context.Background())
	return byName(r.Accounts())
}

// Obligation 7, the successful half: both headers leave, the body lands on
// the account's facts, and it is stored in the Codex store only. Claude
// Code's files are byte-identical afterwards, and an outstanding Claude Code
// quiet period still defers (obligation 1, the Codex half).
func TestCodexRefreshRound(t *testing.T) {
	var mu sync.Mutex
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method %s: headroom only ever reads", r.Method)
		}
		w.Write([]byte(codexUsage("acct-1", "user-1", 37)))
	}))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	l := login(1)
	f.extra(t, l)

	// A Claude Code quiet period and routing fact that must survive untouched.
	claudeStore := state.Open(f.cfg.Claude)
	claudeKey := state.Key{UUID: "claude-uuid", Name: "c@x.com"}
	if decs, err := claudeStore.Claim([]state.Key{claudeKey}, time.Now()); err != nil || !decs[0].Permit {
		t.Fatalf("claude claim: %v %v", decs, err)
	}
	if err := os.WriteFile(f.cfg.Claude.CurrentFile(), []byte("c@x.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeState := filepath.Join(f.cfg.Claude.AccountsRoot, "state.json")
	beforeState, _ := os.ReadFile(claudeState)
	beforeCurrent, _ := os.ReadFile(f.cfg.Claude.CurrentFile())

	byName := f.round(t)
	d, ok := byName["u1@x.com"]
	if !ok {
		t.Fatalf("accounts: %v", byName)
	}
	v := d.View
	if v.Vendor != config.Codex || v.Health != accountstate.HealthOK || v.Attempt.State != accountstate.AttemptOK {
		t.Fatalf("facts: %+v", v)
	}
	if v.Obs == nil || len(v.Obs.Rows) != 1 || v.Obs.Rows[0].Percent != 37 || v.Obs.Source != accountstate.SourceLive ||
		v.Obs.Allowance.State != usage.AllowanceAllowed {
		t.Fatalf("observation: %+v", v.Obs)
	}
	if v.Plan != "prolite" {
		t.Errorf("plan = %q, want the one headroom's own response named", v.Plan)
	}
	if v.Label != "u1@x.com" || v.Launcher != "headroom launch --vendor codex --account u1@x.com" {
		t.Errorf("label %q launcher %q", v.Label, v.Launcher)
	}
	if v.LoginCommand != "headroom launch --vendor codex --account u1@x.com -- login" {
		t.Errorf("login command = %q: every Codex log-in hint is built from it", v.LoginCommand)
	}

	// The primary has no auth.json: no login, and no request left for it
	// (the one request the server saw is u1's, below).
	if p, ok := byName["primary"]; !ok || p.View.Health != accountstate.HealthNoLogin || p.View.Attempt.State != accountstate.AttemptNone {
		t.Errorf("primary: %+v", p)
	}
	if len(got) != 1 {
		t.Fatalf("%d requests left, want exactly one", len(got))
	}
	if h := got[0]; h.Get("Authorization") != "Bearer "+l.AccessToken() || h.Get("ChatGPT-Account-Id") != "acct-1" || h.Get("User-Agent") == "" {
		t.Errorf("request headers: %v", h)
	}

	// Stored in the Codex store, under the two-part key.
	snap := state.Open(f.scope).Load()
	if _, ok := snap.Observation(state.Key{UUID: "acct-1/user-1"}, time.Now()); !ok {
		t.Error("the response is not in the Codex store under uuid:acct-1/user-1")
	}
	if after, _ := os.ReadFile(claudeState); string(after) != string(beforeState) {
		t.Error("a Codex refresh rewrote Claude Code's state.json")
	}
	if after, _ := os.ReadFile(f.cfg.Claude.CurrentFile()); string(after) != string(beforeCurrent) {
		t.Error("a Codex refresh rewrote Claude Code's .current")
	}
	if decs, _ := claudeStore.Claim([]state.Key{claudeKey}, time.Now()); decs[0].Permit {
		t.Error("an outstanding Claude Code quiet period stopped deferring")
	}

	// No second request leaves inside the quiet period: the next round
	// replays the stored response with its source saying so.
	again := f.round(t)["u1@x.com"].View
	if len(got) != 1 {
		t.Errorf("a second request left inside the quiet period (%d total)", len(got))
	}
	if again.Attempt.State != accountstate.AttemptDeferred || again.Obs == nil || again.Obs.Source != accountstate.SourceStore || again.Obs.Rows[0].Percent != 37 {
		t.Errorf("replay: %+v obs %+v", again.Attempt, again.Obs)
	}
	if again.Plan != "prolite" {
		t.Errorf("replayed plan = %q", again.Plan)
	}
}

// A failed request never reads as a failed account, and never erases what is
// known. A 401 in particular stays on the attempt axis: Codex itself recovers
// from one by refreshing, so it is not evidence that a person must log in.
func TestCodexFailedRequestsLeaveHealthAndRowsAlone(t *testing.T) {
	for _, tc := range []struct {
		code int
		want accountstate.AttemptState
	}{
		{http.StatusUnauthorized, accountstate.AttemptHTTP},
		{http.StatusTooManyRequests, accountstate.AttemptRefused},
		{http.StatusBadGateway, accountstate.AttemptHTTP},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			status := http.StatusOK
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if status != http.StatusOK {
					w.WriteHeader(status)
					return
				}
				w.Write([]byte(codexUsage("acct-1", "user-1", 12)))
			}))
			defer srv.Close()
			f := newCodexFixture(t, srv.URL)
			f.scope.Spacing = time.Millisecond // let the second round ask
			f.extra(t, login(1))

			if v := f.round(t)["u1@x.com"].View; v.Attempt.State != accountstate.AttemptOK {
				t.Fatalf("first round: %+v", v.Attempt)
			}
			time.Sleep(5 * time.Millisecond)
			status = tc.code
			v := f.round(t)["u1@x.com"].View
			if v.Attempt.State != tc.want || v.Attempt.HTTPCode != tc.code {
				t.Errorf("attempt = %+v", v.Attempt)
			}
			if v.Health != accountstate.HealthOK {
				t.Errorf("health = %v: a rejected request read as a failed account", v.Health)
			}
			if v.Obs == nil || len(v.Obs.Rows) != 1 || v.Obs.Rows[0].Percent != 12 {
				t.Errorf("rows were not kept: %+v", v.Obs)
			}
			if hits != 2 {
				t.Errorf("%d requests, want 2 — no retry inside a run", hits)
			}
			if tc.code == http.StatusTooManyRequests && v.Attempt.NextEligibleAt <= time.Now().Unix() {
				t.Errorf("a refusal recorded no backoff: %+v", v.Attempt)
			}
		})
	}
}

// The response identity check, on both of its halves, and on replay.
func TestCodexResponseNamingSomeoneElseIsNeverShown(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		shown      bool
	}{
		{"its own", codexUsage("acct-1", "user-1", 5), true},
		{"another account", codexUsage("acct-9", "user-1", 5), false},
		{"another user of the same account", codexUsage("acct-1", "user-9", 5), false},
		{"no identity at all", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":null}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
			defer srv.Close()
			f := newCodexFixture(t, srv.URL)
			f.extra(t, login(1))
			v := f.round(t)["u1@x.com"].View
			if (v.Obs != nil) != tc.shown {
				t.Errorf("live: obs %+v attempt %+v", v.Obs, v.Attempt)
			}
			// Replay applies the same check: a body already in the store under
			// this key is not shown if it names someone else.
			disk := accountstate.Read(f.scope, state.Open(f.scope), time.Now())
			for _, a := range disk.Accounts {
				if a.Acct.Name == "u1@x.com" && (a.View.Obs != nil) != tc.shown {
					t.Errorf("replay: obs %+v", a.View.Obs)
				}
			}
		})
	}

	// A body written into the store by hand under the right key, naming
	// someone else, is still refused on replay.
	f := newCodexFixture(t, "http://127.0.0.1:1")
	f.extra(t, login(1))
	st := state.Open(f.scope)
	key := state.Key{UUID: "acct-1/user-1", Name: "u1@x.com"}
	decs, err := st.Claim([]state.Key{key}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Complete(key, decs[0].Generation, state.OutcomeStored, []byte(codexUsage("acct-9", "user-9", 80)), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, a := range accountstate.Read(f.scope, st, time.Now()).Accounts {
		if a.View.Obs != nil {
			t.Errorf("%s: a stored body naming someone else was replayed", a.Acct.Name)
		}
	}
}

// A Codex account with an unknown identity has no ledger key at all: it makes
// no claim and replays nothing, even if an earlier login on that home was
// fetched under the dir's name.
func TestCodexUnknownIdentityHasNoLedgerKey(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1")
	dir := filepath.Join(f.scope.AccountsRoot, "noident@x.com")
	codexauthtest.WriteRaw(t, dir, codexauthtest.Login{Email: "noident@x.com", AccountID: "acct-7", Token: "t"}.Document())
	st := state.Open(f.scope)
	stray := state.Key{Name: "noident@x.com"} // the dir: fallback Claude Code accounts use
	decs, err := st.Claim([]state.Key{stray}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Complete(stray, decs[0].Generation, state.OutcomeStored, []byte(codexUsage("", "", 9)), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, a := range accountstate.Read(f.scope, st, time.Now()).Accounts {
		if a.Acct.Name == "noident@x.com" && (a.View.Obs != nil || a.View.Attempt.NextEligibleAt != 0) {
			t.Errorf("an unidentified account replayed through the dir: fallback: %+v", a.View)
		}
	}
}

// Obligation 8: no usage is derived from Codex's session files. A home whose
// sessions/ holds rollouts with rate_limits records, and no stored response,
// renders usage unknown.
func TestCodexRolloutsAreNeverUsage(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1")
	f.extra(t, login(1))
	day := filepath.Join(f.scope.StoreDir(), "2026", "09", "21")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := `{"timestamp":"2026-09-21T08:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":64,"window_minutes":10080,"resets_at":1790239759},"plan_type":"pro"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-09-21T08-00-00-01a0c32b.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range accountstate.Read(f.scope, state.Open(f.scope), time.Now()).Accounts {
		if a.View.Obs != nil {
			t.Errorf("%s: usage appeared from a rollout: %+v", a.Acct.Name, a.View.Obs)
		}
	}
}

// The ledger key's spelling is permanent: a quiet period written under the
// shipped key must still defer. The document is seeded literally, because a
// test that writes and reads through Key.ID would follow a respelling.
func TestCodexLedgerKeySpellingIsPermanent(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(codexUsage("acct-1", "user-1", 5)))
	}))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	f.extra(t, login(1))
	now := time.Now()
	doc := fmt.Sprintf(`{"version":1,"accounts":{"uuid:acct-1/user-1":{"name":"u1@x.com","request":{"last_attempt_ms":%d,"next_eligible_ms":%d}}}}`,
		now.UnixMilli(), now.Add(time.Minute).UnixMilli())
	if err := os.MkdirAll(f.scope.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.scope.AccountsRoot, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	v := f.round(t)["u1@x.com"].View
	if hits != 0 || v.Attempt.State != accountstate.AttemptDeferred {
		t.Errorf("a quiet period under uuid:acct-1/user-1 was not honoured: %d request(s), attempt %+v", hits, v.Attempt)
	}
}

// Spacing is the scope's, and it reaches the consumers that used to read one
// global constant: the facts' fresh window and the idle board's cadence.
func TestScopeSpacingReachesFreshnessAndTheIdleCadence(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1")
	f.scope.Spacing = 7 * time.Minute
	f.extra(t, login(1))
	st := state.Open(f.scope)
	for _, a := range accountstate.Read(f.scope, st, time.Now()).Accounts {
		if a.View.FreshFor != 7*time.Minute {
			t.Errorf("%s: fresh window %v, want the scope's spacing", a.Acct.Name, a.View.FreshFor)
		}
	}
	pg := &page{scope: f.scope, st: st} // nothing to ask about: the idle cadence
	pg.schedule()
	if wait := time.Until(pg.nextAt); wait < 6*time.Minute || wait > 8*time.Minute {
		t.Errorf("idle cadence %v, want the scope's spacing", wait)
	}
}
