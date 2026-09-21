package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// resolve writes one view and nothing else — the store has already been told,
// by the goroutine that fetched. A successful observation must arrive stamped:
// rows without a time were the mechanism by which carried-over data passed
// itself off as current.
func TestResolveKeepsReceivedTimestamp(t *testing.T) {
	now := time.Now()
	out := refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptOK}, Observation: &accountstate.Observation{Rows: []usage.Row{{Percent: 5}, {Percent: 9}}, ObservedAt: now.Add(-5 * time.Second).Unix(), Source: accountstate.SourceLive}}

	d := &accountData{}
	resolve(d, out)
	if d.View.Obs == nil || len(d.View.Obs.Rows) != 2 {
		t.Fatalf("rows not carried: %+v", d.View.Obs)
	}
	if d.View.Obs.ObservedAt != now.Add(-5*time.Second).Unix() || d.View.Obs.Source != accountstate.SourceLive {
		t.Errorf("observation lacks provenance: %+v", d.View.Obs)
	}
}

// The headline regression: a refused request says nothing about the account,
// so it must annotate what is known rather than erase it. Before the three-axis
// model, a 429 replaced the rows and the whole board read as broken.
func TestRefusalKeepsObservation(t *testing.T) {
	now := time.Now()
	prior := &accountstate.Observation{
		Rows:       []usage.Row{{Label: "5h session", Percent: 42}},
		ObservedAt: now.Add(-30 * time.Second).Unix(),
		Source:     accountstate.SourceLive,
	}
	d := &accountData{View: accountstate.Facts{Obs: prior}}
	resolve(d, refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, NextEligibleAt: now.Add(state.CooldownBase).Unix()}})

	if d.View.Obs != prior {
		t.Fatalf("429 dropped the observation: %+v", d.View.Obs)
	}
	if d.View.Obs.ObservedAt != prior.ObservedAt {
		t.Errorf("429 restamped the observation as if it were fresh")
	}
	if d.View.Attempt.State != accountstate.AttemptRefused {
		t.Errorf("attempt = %v, want refused", d.View.Attempt.State)
	}
	if d.View.Health != accountstate.HealthOK {
		t.Errorf("a refused request must not change health: %v", d.View.Health)
	}
	// And it must schedule its own quiet period rather than hammering.
	if d.View.Attempt.NextEligibleAt <= now.Unix() {
		t.Errorf("no cooldown scheduled after 429: %+v", d.View.Attempt)
	}
}

// Transport failures get the same protection: a flaky network is not news
// about the account either.
func TestTransportFailureKeepsObservation(t *testing.T) {
	now := time.Now()
	prior := &accountstate.Observation{Rows: []usage.Row{{Label: "5h session", Percent: 7}},
		ObservedAt: now.Add(-time.Minute).Unix(), Source: accountstate.SourceCache}
	d := &accountData{View: accountstate.Facts{Obs: prior}}
	out := refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptTransport}}
	resolve(d, out)
	if d.View.Obs != prior || d.View.Attempt.State != accountstate.AttemptTransport {
		t.Errorf("transport failure mishandled: obs=%+v attempt=%+v", d.View.Obs, d.View.Attempt)
	}
}

// Regression test for the picker data race: the consumer applies each result
// and then — like runSelect's draw — reads every account's view, while other
// fetches are still in flight. Fetch goroutines writing views themselves
// made this fail under -race; launchFetches must keep views single-writer.
func TestLaunchFetchesSingleWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "slow") {
			time.Sleep(50 * time.Millisecond)
		}
		w.Write([]byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`))
	}))
	defer srv.Close()

	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	list := []*accountData{
		{Request: candidate(srv.URL, "fast", "fast"),
			View: accountstate.Facts{Attempt: accountstate.Attempt{State: accountstate.AttemptPending}}},
		{Request: candidate(srv.URL, "slow", "slow"),
			View: accountstate.Facts{Attempt: accountstate.Attempt{State: accountstate.AttemptPending}}},
	}
	list[0].Acct.Name, list[1].Acct.Name = "fast", "slow"
	for u := range launchFetches(context.Background(), list, st) {
		resolve(list[u.Index], u)
		for _, d := range list {
			_ = d.View.Attempt.State
			if d.View.Obs != nil {
				_ = len(d.View.Obs.Rows)
			}
		}
	}
	for i, d := range list {
		if d.View.Attempt.State != accountstate.AttemptOK {
			t.Errorf("account %d not resolved: attempt %v", i, d.View.Attempt.State)
		}
	}
}

// Health answers "can Claude Code use this account". A credential headroom
// can't read stops *headroom* from fetching; it says nothing about the
// account, and reporting it as an account problem is the same false alarm
// this rework exists to remove.
func TestBadBlobIsNotAnAccountHealthProblem(t *testing.T) {
	ok := auth.Status{LoggedIn: true, Outcome: auth.OutcomeOK}
	if got := resolveHealth(ok, `not json`, creds.Blob{}, false, 0); got != accountstate.HealthOK {
		t.Errorf("unreadable credential downgraded a logged-in account: health=%v", got)
	}
	if got := resolveHealth(ok, "", creds.Blob{}, false, 0); got != accountstate.HealthOK {
		t.Errorf("missing credential downgraded a logged-in account: health=%v", got)
	}
	// With no first-party answer, credential evidence still decides.
	if got := resolveHealth(auth.Status{}, "", creds.Blob{}, false, 0); got != accountstate.HealthNoLogin {
		t.Errorf("no auth answer + no credential should be no-login: health=%v", got)
	}
	if got := resolveHealth(auth.Status{}, `not json`, creds.Blob{}, false, 0); got != accountstate.HealthBadBlob {
		t.Errorf("no auth answer + bad blob should surface drift: health=%v", got)
	}
}

// Whatever went wrong reading the credential must still reach the user, just
// on the axis it belongs to.
func TestUnreadableCredentialBecomesAnAttemptFact(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PrimaryMeta(),
		[]byte(`{"oauthAccount":{"emailAddress":"p@x.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	list, _ := prepareWith(accounts.Discover(cfg), state.Open(cfg).Load(), sources{
		readRaw: func(string) string { return `not json` },
		health:  func(string) auth.Status { return auth.Status{LoggedIn: true, Outcome: auth.OutcomeOK} },
		now:     time.Now(),
	})
	v := list[0].View
	if v.Health != accountstate.HealthOK {
		t.Errorf("health should stay OK: %v", v.Health)
	}
	if v.Attempt.State != accountstate.AttemptCredentialUnreadable {
		t.Errorf("attempt should carry the unreadable credential: %v", v.Attempt.State)
	}
	if list[0].Request != nil {
		t.Error("must not fetch without a usable credential")
	}
}

// Zero limit rows is a documented, contractual answer — usage.ParseLimits
// defines it as "this account reports no limits". It is newer truth than any
// cached bars and must replace them, not hide behind them.
func TestZeroRowsBecomesTheNewestObservation(t *testing.T) {
	now := time.Now()
	stale := &accountstate.Observation{
		Rows:       []usage.Row{{Label: "5h session", Percent: 58}},
		ObservedAt: now.Add(-22 * time.Hour).Unix(), Source: accountstate.SourceCache,
	}
	d := &accountData{View: accountstate.Facts{Obs: stale}}
	d.Acct.Name = "a"
	out := refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptNoLimits}, Observation: &accountstate.Observation{ObservedAt: now.Unix(), Source: accountstate.SourceLive}}
	resolve(d, out)

	if d.View.Obs == stale {
		t.Fatal("22h-old bars still displayed after the endpoint reported no limits")
	}
	if d.View.Obs == nil || len(d.View.Obs.Rows) != 0 || d.View.Obs.ObservedAt != now.Unix() {
		t.Errorf("zero-row observation not recorded: %+v", d.View.Obs)
	}
}

// oneRound is what every surface does: prepare, claim, fetch, resolve. A fresh
// store handle each time, because every surface is a fresh process — and the
// bugs below only appear across two rounds, since the first is what leaves the
// ledger in the state the second one reads.
func oneRound(t *testing.T, cfg config.Scope, blobs map[string]string, now time.Time) map[string]*accountData {
	t.Helper()
	st := state.Open(cfg)
	list, _ := prepareWith(accounts.Discover(cfg), st.Load(), sources{
		readRaw: func(dir string) string { return blobs[dir] },
		health:  func(string) auth.Status { return auth.Status{} },
		now:     now,
	})
	for u := range launchFetches(context.Background(), list, st) {
		resolve(list[u.Index], u)
	}
	byName := map[string]*accountData{}
	for _, d := range list {
		byName[d.Acct.Name] = d
	}
	return byName
}

// oneAccount builds a home with the primary logged out and one usable account,
// and returns the config plus the credential map prepare reads.
func oneAccount(t *testing.T, usageURL, meta string) (config.Scope, map[string]string) {
	t.Helper()
	home := t.TempDir()
	cfg := withUsageURL(claudeScope(home, "primary"), usageURL)
	dir := filepath.Join(cfg.AccountsRoot, "a@x.com")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, ".claude.json"), meta)
	writeJSON(t, cfg.PrimaryMeta(), `{}`)
	return cfg, map[string]string{dir: `{"claudeAiOauth":{"accessToken":"tok"}}`}
}

const goodMeta = `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"uuid-a"}}`

func usageServer(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"limits":[{"kind":"session","percent":5}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The quarantine's promise is that an unreadable ledger leaves every account
// quiet for one cooldown rather than bricking. That promise is kept by Claim —
// which only ever sees the accounts prepare marked as wanting a fetch, and
// prepare decides that by reading the very ledger that is unreadable. Two runs
// is the whole point: the first looks correct in isolation.
func TestACorruptLedgerDoesNotFanOutOnTheNextRun(t *testing.T) {
	var requests atomic.Int32
	srv := usageServer(t, &requests)
	cfg, blobs := oneAccount(t, srv.URL, goodMeta)
	writeJSON(t, filepath.Join(cfg.AccountsRoot, "state.json"), `{"version":1,"accounts":5}`)

	now := time.Now()
	first := oneRound(t, cfg, blobs, now)
	oneRound(t, cfg, blobs, now.Add(time.Second))

	if n := requests.Load(); n != 0 {
		t.Errorf("an unreadable ledger issued %d request(s); the quarantine is supposed to "+
			"leave every account quiet for one cooldown", n)
	}
	// And the run that met the corruption must say so as headroom's own
	// problem: "live check deferred" is a statement about the endpoint's
	// budget, and the endpoint has said nothing. (The run after it is
	// ordinarily deferred — by then the quarantine has left a real cooldown
	// record, and `check` is where the corruption itself gets reported.)
	if got := first["a@x.com"].View.Attempt.State; got != accountstate.AttemptStateUnavailable {
		t.Errorf("attempt = %v, want state_unavailable: the ledger could not be read, "+
			"so nothing here is news about the account's budget", got)
	}
}

// A document written by a newer headroom is headroom's own problem, and the
// row must say so. "live check deferred, next attempt in 16m" is a statement
// about the endpoint's budget, and the endpoint has said nothing.
func TestANewerSchemaIsNotReportedAsADeferredRequest(t *testing.T) {
	var requests atomic.Int32
	srv := usageServer(t, &requests)
	cfg, blobs := oneAccount(t, srv.URL, goodMeta)
	writeJSON(t, filepath.Join(cfg.AccountsRoot, "state.json"), `{"version":999,"accounts":{}}`)

	got := oneRound(t, cfg, blobs, time.Now())["a@x.com"].View.Attempt.State
	if got != accountstate.AttemptStateUnavailable {
		t.Errorf("attempt = %v, want state_unavailable: the request was never refused, "+
			"headroom refused to write a schema it cannot read", got)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d request(s) issued with no writable ledger to claim against", n)
	}
}

// The ledger keys on the account's own UUID, which comes from a vendor file
// Claude Code rewrites constantly. A torn read of it flips the key — and
// anything that sweeps the ledger by absence then deletes a live cooldown and
// the observation this whole store exists to keep.
func TestATornAccountFileDoesNotCostTheStoredObservation(t *testing.T) {
	var requests atomic.Int32
	srv := usageServer(t, &requests)
	cfg, blobs := oneAccount(t, srv.URL, goodMeta)
	metaPath := filepath.Join(cfg.AccountsRoot, "a@x.com", ".claude.json")
	now := time.Now()

	oneRound(t, cfg, blobs, now) // asks once, stores the answer under uuid:uuid-a
	writeJSON(t, metaPath, `{ torn`)
	oneRound(t, cfg, blobs, now.Add(time.Second)) // keyed dir:a@x.com for this run only
	writeJSON(t, metaPath, goodMeta)
	back := oneRound(t, cfg, blobs, now.Add(2*time.Second))["a@x.com"]

	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests in three seconds for one account, want 1: the budget is per "+
			"account, so a key that moves because a vendor file was read mid-write must "+
			"not buy a second bucket", n)
	}
	if back.View.Obs == nil || back.View.Obs.Source != accountstate.SourceStore {
		t.Errorf("the stored observation did not survive the torn read: %+v", back.View.Obs)
	}
}

// The retired verb's tombstone: exit 2, actionable stderr, and nothing on
// stdout a positional reader could consume — its stdout was a decision
// protocol, and a stale shell function is exactly who still calls it.
func TestResumeSpellingIsTombstoned(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	code := Run([]string{"resume"})
	w.Close()
	os.Stdout = prev
	out := make([]byte, 64)
	n, _ := r.Read(out)

	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if n != 0 {
		t.Errorf("stdout carried %q — must be empty", out[:n])
	}
	// --json retires with it: one name, one meaning, across generations.
	if code := Run([]string{"resume", "--json"}); code != 2 {
		t.Errorf("resume --json: exit %d, want 2", code)
	}
}

// The board's presentation flag is opt-in: absent, the layout is the blocks
// whatever else is typed, and what follows the flag is left for the
// no-arguments check to refuse rather than swallowed.
func TestBoardLayoutFlagIsOptIn(t *testing.T) {
	if l, rest := boardLayout(nil); l != render.LayoutBlocks || len(rest) != 0 {
		t.Fatalf("bare board = %v %v, want blocks with nothing left", l, rest)
	}
	if l, rest := boardLayout([]string{"--compact"}); l != render.LayoutCompact || len(rest) != 0 {
		t.Fatalf("--compact = %v %v, want compact with nothing left", l, rest)
	}
	if l, rest := boardLayout([]string{"--compact", "extra"}); l != render.LayoutCompact || len(rest) != 1 {
		t.Fatalf("--compact extra = %v %v, want compact with the stray argument left to refuse", l, rest)
	}
	if l, rest := boardLayout([]string{"stray"}); l != render.LayoutBlocks || len(rest) != 1 {
		t.Fatalf("stray = %v %v, want blocks with the argument left to refuse", l, rest)
	}
}

func candidate(url, name, token string) *refresh.Candidate {
	c, _ := refresh.Prepare(accounts.Account{Scope: config.Scope{UsageURL: url}, Name: name, Readable: true}, creds.Blob{Token: token, ExpiresAtMS: time.Now().Add(time.Hour).UnixMilli()}, true, time.Now())
	return c
}

func TestPersistenceFailureKeepsEndpointVerdict(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		state  accountstate.AttemptState
		phrase string
	}{
		{accountstate.AttemptOK, ""}, {accountstate.AttemptRefused, "rate limited"},
	} {
		d := &accountData{Acct: accounts.Account{Name: "a"}, View: accountstate.Facts{Obs: &accountstate.Observation{Rows: []usage.Row{{Percent: 42}}, ObservedAt: now.Unix(), Source: accountstate.SourceLive}}}
		resolve(d, refresh.Result{Attempt: accountstate.Attempt{State: tc.state}, StoreErr: state.ErrReadOnly})
		if d.View.Attempt.State != tc.state {
			t.Errorf("persistence replaced endpoint verdict: %v", d.View.Attempt.State)
		}
		for _, layout := range []render.Layout{render.LayoutBlocks, render.LayoutCompact} {
			text := strings.Join(render.NewPalette(false).Board(views([]*accountData{d}), now.Unix(), layout, 0).Groups[0], "\n")
			if !strings.Contains(text, "state file unavailable") || !strings.Contains(text, tc.phrase) {
				t.Errorf("missing independent evidence: %s", text)
			}
		}
		data, err := jsonDocument(claudeBoard([]*accountData{d}, "a"), now)
		if err != nil || !strings.Contains(string(data), `"problems"`) {
			t.Errorf("JSON omitted bookkeeping problem: %s %v", data, err)
		}
	}
}

func TestSuccessfulRefreshPresentation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		empty      bool
	}{
		{"rows", `{"limits":[{"kind":"session","percent":42}]}`, false},
		{"empty", `{"limits":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			d := &accountData{Request: candidate(srv.URL, "a", "token")}
			for r := range launchFetches(context.Background(), []*accountData{d}, state.Open(config.Scope{AccountsRoot: t.TempDir()})) {
				resolve(d, r)
			}
			ui := page{list: []*accountData{d}}
			if got := ui.ackString(time.Now()); got != "refreshed · all current" {
				t.Errorf("ack=%q", got)
			}
			data, err := jsonDocument(claudeBoard(ui.list, ""), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"http_status"`) || (strings.Contains(string(data), `"next_eligible_at"`) != tc.empty) {
				t.Errorf("successful attempt gained error/retry fields: %s", data)
			}
		})
	}
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	defer func() { os.Stderr = previous; f.Close() }()
	os.Stderr = f
	run()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBoardLeaves401CredentialSamplingToChecker(t *testing.T) {
	bin := t.TempDir()
	sampled := filepath.Join(t.TempDir(), "sampled")
	t.Setenv("PATH", bin)
	t.Setenv("HEADROOM_TEST_CREDENTIAL_SAMPLE", sampled)
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\necho sampled > \"$HEADROOM_TEST_CREDENTIAL_SAMPLE\"\nexit 44\n"), 0755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	d := &accountData{Request: candidate(srv.URL, "a", "token")}
	for r := range launchFetches(context.Background(), []*accountData{d}, state.Open(config.Scope{AccountsRoot: t.TempDir()})) {
		if r.Attempt.HTTPCode != 401 || r.TokenAfter401 != refresh.TokenUnknown {
			t.Fatalf("board's evidence=%+v", r)
		}
	}
	if _, err := os.Stat(sampled); !os.IsNotExist(err) {
		t.Fatal("board sampled credentials for diagnostic evidence it does not consume")
	}
}
