package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// classify turns one result into two verdicts: what the user is told, and what
// the ledger is told. The second column used to be buried inside resolve and
// asserted nowhere — yet it is the one that decides whether a strike is
// recorded, and a 200 nobody could parse still proves the budget recovered.
func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		res     usage.Result
		attempt render.AttemptState
		outcome state.Outcome
	}{
		{"transport error", usage.Result{Err: errors.New("timeout")},
			render.AttemptTransport, state.OutcomeFailed},
		{"rate limited", usage.Result{StatusCode: 429},
			render.AttemptRefused, state.OutcomeRefused},
		{"auth rejected", usage.Result{StatusCode: http.StatusUnauthorized},
			render.AttemptHTTP, state.OutcomeFailed},
		{"unparseable body", usage.Result{StatusCode: 200, Body: []byte(`nope`)},
			render.AttemptUnparseable, state.OutcomeSpent},
		{"no limits", usage.Result{StatusCode: 200, Body: []byte(`{}`)},
			render.AttemptNoLimits, state.OutcomeStored},
		{"rows", usage.Result{StatusCode: 200,
			Body: []byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`)},
			render.AttemptOK, state.OutcomeStored},
	}
	for _, c := range cases {
		out, outcome, body := classify(c.res)
		if out.attempt != c.attempt {
			t.Errorf("%s: attempt = %v, want %v", c.name, out.attempt, c.attempt)
		}
		if outcome != c.outcome {
			t.Errorf("%s: ledger outcome = %v, want %v", c.name, outcome, c.outcome)
		}
		// Only a body that parsed is worth keeping, and everything that
		// parsed is: zero rows is a contractual answer, not an absence.
		if stored := len(body) > 0; stored != (outcome == state.OutcomeStored) {
			t.Errorf("%s: stored %v against outcome %v", c.name, stored, outcome)
		}
	}

	if out, _, _ := classify(usage.Result{StatusCode: http.StatusUnauthorized}); out.code != http.StatusUnauthorized {
		t.Errorf("HTTP code not carried: %+v", out)
	}
}

// resolve writes one view and nothing else — the store has already been told,
// by the goroutine that fetched. A successful observation must arrive stamped:
// rows without a time were the mechanism by which carried-over data passed
// itself off as current.
func TestResolveStampsTheObservation(t *testing.T) {
	now := time.Now()
	out, _, _ := classify(usage.Result{StatusCode: 200,
		Body: []byte(`{"limits":[{"kind":"session","percent":5},{"group":"weekly","percent":9}]}`)})

	d := &accountData{}
	resolve(d, fetchUpdate{out: out}, now)
	if d.View.Obs == nil || len(d.View.Obs.Rows) != 2 {
		t.Fatalf("rows not carried: %+v", d.View.Obs)
	}
	if d.View.Obs.ObservedAt != now.Unix() || d.View.Obs.Source != render.SourceLive {
		t.Errorf("observation lacks provenance: %+v", d.View.Obs)
	}
}

// The headline regression: a refused request says nothing about the account,
// so it must annotate what is known rather than erase it. Before the three-axis
// model, a 429 replaced the rows and the whole board read as broken.
func TestRefusalKeepsObservation(t *testing.T) {
	now := time.Now()
	prior := &render.Observation{
		Rows:       []usage.Row{{Label: "5h session", Percent: 42}},
		ObservedAt: now.Add(-30 * time.Second).Unix(),
		Source:     render.SourceLive,
	}
	// Through a real claim and a real completion: the cooldown a refusal earns
	// belongs to the request that was authorized, and only a matching
	// generation may collect it.
	st := state.Open(t.TempDir())
	key := state.Key{Name: "a"}
	dec, err := st.Claim([]state.Key{key}, now)
	if err != nil {
		t.Fatal(err)
	}
	out, outcome, body := classify(usage.Result{StatusCode: 429})
	next, err := st.Complete(key, dec[0].Generation, outcome, body, now)
	if err != nil {
		t.Fatal(err)
	}
	d := &accountData{Key: key, View: render.AccountView{Obs: prior}}
	resolve(d, fetchUpdate{out: out, next: next}, now)

	if d.View.Obs != prior {
		t.Fatalf("429 dropped the observation: %+v", d.View.Obs)
	}
	if d.View.Obs.ObservedAt != prior.ObservedAt {
		t.Errorf("429 restamped the observation as if it were fresh")
	}
	if d.View.Attempt.State != render.AttemptRefused {
		t.Errorf("attempt = %v, want refused", d.View.Attempt.State)
	}
	if d.View.Health != render.HealthOK {
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
	prior := &render.Observation{Rows: []usage.Row{{Label: "5h session", Percent: 7}},
		ObservedAt: now.Add(-time.Minute).Unix(), Source: render.SourceCache}
	d := &accountData{View: render.AccountView{Obs: prior}}
	out, _, _ := classify(usage.Result{Err: errors.New("dial tcp: no route")})
	resolve(d, fetchUpdate{out: out}, now)
	if d.View.Obs != prior || d.View.Attempt.State != render.AttemptTransport {
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

	cfg := config.Config{UsageURL: srv.URL, AccountsRoot: t.TempDir()}
	st := state.Open(cfg.AccountsRoot)
	list := []*accountData{
		{Token: "fast", Key: state.Key{Name: "fast"}, WantsFetch: true,
			View: render.AccountView{Attempt: render.Attempt{State: render.AttemptPending}}},
		{Token: "slow", Key: state.Key{Name: "slow"}, WantsFetch: true,
			View: render.AccountView{Attempt: render.Attempt{State: render.AttemptPending}}},
	}
	list[0].Acct.Name, list[1].Acct.Name = "fast", "slow"
	for u := range launchFetches(context.Background(), cfg, list, st, 1) {
		resolve(list[u.idx], u, time.Now())
		for _, d := range list {
			_ = d.View.Attempt.State
			if d.View.Obs != nil {
				_ = len(d.View.Obs.Rows)
			}
		}
	}
	for i, d := range list {
		if d.View.Attempt.State != render.AttemptOK {
			t.Errorf("account %d not resolved: attempt %v", i, d.View.Attempt.State)
		}
	}
}

// launchFetches must claim each account's budget before its request leaves,
// so a second process starting mid-round sees it spent. Recording only on
// completion would let two runs fetch the same account back to back — the
// pattern that produced the fleet-wide refusals.
//
// The check has to happen at the moment the request is *served*: asserting
// after launchFetches returns proves nothing, because the save has landed by
// then. The window this pins is between the request leaving and the claim
// reaching disk.
func TestFetchClaimsBudgetBeforeRequesting(t *testing.T) {
	root := t.TempDir()
	released := make(chan struct{})
	var eligibleAtRequestTime bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What a separate process would see, reading the store off disk right
		// as this request arrives.
		now := time.Now()
		eligibleAtRequestTime = !state.Open(root).Load().
			NextEligible(state.Key{Name: "acct"}, now).After(now)
		<-released
		w.Write([]byte(`{"limits":[{"kind":"session","percent":1}]}`))
	}))
	defer srv.Close()

	cfg := config.Config{UsageURL: srv.URL, AccountsRoot: root}
	st := state.Open(root)
	list := []*accountData{{Token: "t", Key: state.Key{Name: "acct"}, WantsFetch: true}}
	list[0].Acct.Name = "acct"

	updates := launchFetches(context.Background(), cfg, list, st, 1)
	close(released)
	for range updates {
	}
	if eligibleAtRequestTime {
		t.Error("the claim was not on disk when the request was served: " +
			"a concurrent process would have spent the same account's budget")
	}
}

// Health answers "can Claude Code use this account". A credential headroom
// can't read stops *headroom* from fetching; it says nothing about the
// account, and reporting it as an account problem is the same false alarm
// this rework exists to remove.
func TestBadBlobIsNotAnAccountHealthProblem(t *testing.T) {
	ok := auth.Status{LoggedIn: true, Outcome: auth.OutcomeOK}
	if got := resolveHealth(ok, `not json`, creds.Blob{}, false, 0); got != render.HealthOK {
		t.Errorf("unreadable credential downgraded a logged-in account: health=%v", got)
	}
	if got := resolveHealth(ok, "", creds.Blob{}, false, 0); got != render.HealthOK {
		t.Errorf("missing credential downgraded a logged-in account: health=%v", got)
	}
	// With no first-party answer, credential evidence still decides.
	if got := resolveHealth(auth.Status{}, "", creds.Blob{}, false, 0); got != render.HealthNoLogin {
		t.Errorf("no auth answer + no credential should be no-login: health=%v", got)
	}
	if got := resolveHealth(auth.Status{}, `not json`, creds.Blob{}, false, 0); got != render.HealthBadBlob {
		t.Errorf("no auth answer + bad blob should surface drift: health=%v", got)
	}
}

// Whatever went wrong reading the credential must still reach the user, just
// on the axis it belongs to.
func TestUnreadableCredentialBecomesAnAttemptFact(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: home + "/.claude-accounts", PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PrimaryMeta(),
		[]byte(`{"oauthAccount":{"emailAddress":"p@x.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	list, _ := prepareWith(cfg, accounts.Discover(cfg), state.Open(cfg.AccountsRoot).Load(), sources{
		readRaw: func(string) string { return `not json` },
		health:  func(string) auth.Status { return auth.Status{LoggedIn: true, Outcome: auth.OutcomeOK} },
		now:     time.Now(),
	})
	v := list[0].View
	if v.Health != render.HealthOK {
		t.Errorf("health should stay OK: %v", v.Health)
	}
	if v.Attempt.State != render.AttemptCredentialUnreadable {
		t.Errorf("attempt should carry the unreadable credential: %v", v.Attempt.State)
	}
	if list[0].WantsFetch {
		t.Error("must not fetch without a usable credential")
	}
}

// A 200 proves the account's request budget recovered, whatever its body
// turns out to say. Withholding the strike reset until rows parse makes a
// later refusal escalate as though refusals had been consecutive.
func TestAnyHTTP200ClearsRefusalStrikes(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli())
	key := state.Key{Name: "a"}
	// refuse drives one full claim→refusal cycle and reports the cooldown it
	// earned; escalation is visible as that cooldown growing.
	refuse := func(st *state.Store, at time.Time) time.Duration {
		t.Helper()
		dec, err := st.Claim([]state.Key{key}, at)
		if err != nil || !dec[0].Permit {
			t.Fatalf("claim at %v: %v (permit %v)", at, err, dec[0].Permit)
		}
		next, err := st.Complete(key, dec[0].Generation, state.OutcomeRefused, nil, at)
		if err != nil {
			t.Fatal(err)
		}
		return next.Sub(at)
	}
	for _, body := range []string{`{}`, `nonsense`} {
		st := state.Open(t.TempDir())
		refuse(st, now)
		if got := refuse(st, now.Add(time.Hour)); got != 2*state.CooldownBase {
			t.Fatalf("body %q: consecutive refusals must escalate; got %v", body, got)
		}

		at := now.Add(2 * time.Hour)
		dec, _ := st.Claim([]state.Key{key}, at)
		_, outcome, stored := classify(usage.Result{StatusCode: 200, Body: []byte(body)})
		if _, err := st.Complete(key, dec[0].Generation, outcome, stored, at); err != nil {
			t.Fatal(err)
		}

		if got := refuse(st, at.Add(time.Hour)); got != state.CooldownBase {
			t.Errorf("body %q: strikes survived a 200; cooldown %v, want the base %v",
				body, got, state.CooldownBase)
		}
	}
}

// Zero limit rows is a documented, contractual answer — usage.ParseLimits
// defines it as "this account reports no limits". It is newer truth than any
// cached bars and must replace them, not hide behind them.
func TestZeroRowsBecomesTheNewestObservation(t *testing.T) {
	now := time.Now()
	stale := &render.Observation{
		Rows:       []usage.Row{{Label: "5h session", Percent: 58}},
		ObservedAt: now.Add(-22 * time.Hour).Unix(), Source: render.SourceCache,
	}
	d := &accountData{View: render.AccountView{Obs: stale}}
	d.Acct.Name = "a"
	out, _, _ := classify(usage.Result{StatusCode: 200, Body: []byte(`{}`)})
	resolve(d, fetchUpdate{out: out}, now)

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
func oneRound(t *testing.T, cfg config.Config, blobs map[string]string, now time.Time) map[string]*accountData {
	t.Helper()
	st := state.Open(cfg.AccountsRoot)
	list, _ := prepareWith(cfg, accounts.Discover(cfg), st.Load(), sources{
		readRaw: func(dir string) string { return blobs[dir] },
		health:  func(string) auth.Status { return auth.Status{} },
		now:     now,
	})
	for u := range launchFetches(context.Background(), cfg, list, st, 1) {
		resolve(list[u.idx], u, now)
	}
	byName := map[string]*accountData{}
	for _, d := range list {
		byName[d.Acct.Name] = d
	}
	return byName
}

// oneAccount builds a home with the primary logged out and one usable account,
// and returns the config plus the credential map prepare reads.
func oneAccount(t *testing.T, usageURL, meta string) (config.Config, map[string]string) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "primary",
		UsageURL:     usageURL,
	}
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
	if got := first["a@x.com"].View.Attempt.State; got != render.AttemptStateUnavailable {
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
	if got != render.AttemptStateUnavailable {
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
	if back.View.Obs == nil || back.View.Obs.Source != render.SourceStore {
		t.Errorf("the stored observation did not survive the torn read: %+v", back.View.Obs)
	}
}
