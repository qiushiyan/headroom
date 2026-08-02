package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/throttle"
	"github.com/qiushiyan/headroom/internal/usage"
)

func TestResolveAttempt(t *testing.T) {
	cases := []struct {
		name string
		res  usage.Result
		want render.AttemptState
	}{
		{"transport error", usage.Result{Err: errors.New("timeout")}, render.AttemptTransport},
		{"rate limited", usage.Result{StatusCode: 429}, render.AttemptRefused},
		{"auth rejected", usage.Result{StatusCode: http.StatusUnauthorized}, render.AttemptHTTP},
		{"unparseable body", usage.Result{StatusCode: 200, Body: []byte(`nope`)}, render.AttemptUnparseable},
		{"no limits", usage.Result{StatusCode: 200, Body: []byte(`{}`)}, render.AttemptNoLimits},
		{"rows", usage.Result{StatusCode: 200,
			Body: []byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`)},
			render.AttemptOK},
	}
	for _, c := range cases {
		d := &accountData{}
		resolve(d, c.res, throttle.Load(t.TempDir()), time.Now())
		if d.View.Attempt.State != c.want {
			t.Errorf("%s: attempt = %v, want %v", c.name, d.View.Attempt.State, c.want)
		}
	}

	d := &accountData{}
	resolve(d, usage.Result{StatusCode: http.StatusUnauthorized}, throttle.Load(t.TempDir()), time.Now())
	if d.View.Attempt.HTTPCode != http.StatusUnauthorized {
		t.Errorf("HTTP code not carried: %+v", d.View.Attempt)
	}

	// A successful observation must arrive stamped. Rows without a time were
	// the mechanism by which carried-over data passed itself off as current.
	now := time.Now()
	d = &accountData{}
	resolve(d, usage.Result{StatusCode: 200,
		Body: []byte(`{"limits":[{"kind":"session","percent":5},{"group":"weekly","percent":9}]}`)},
		throttle.Load(t.TempDir()), now)
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
	d := &accountData{View: render.AccountView{Obs: prior}}
	resolve(d, usage.Result{StatusCode: 429}, throttle.Load(t.TempDir()), now)

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
	resolve(d, usage.Result{Err: errors.New("dial tcp: no route")}, throttle.Load(t.TempDir()), now)
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
	th := throttle.Load(cfg.AccountsRoot)
	list := []*accountData{
		{Token: "fast", NeedsFetch: true, View: render.AccountView{Attempt: render.Attempt{State: render.AttemptPending}}},
		{Token: "slow", NeedsFetch: true, View: render.AccountView{Attempt: render.Attempt{State: render.AttemptPending}}},
	}
	list[0].Acct.Name, list[1].Acct.Name = "fast", "slow"
	for u := range launchFetches(context.Background(), cfg, list, th) {
		resolve(list[u.idx], u.res, th, time.Now())
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
		eligibleAtRequestTime = throttle.Load(root).Eligible("acct", time.Now())
		<-released
		w.Write([]byte(`{"limits":[{"kind":"session","percent":1}]}`))
	}))
	defer srv.Close()

	cfg := config.Config{UsageURL: srv.URL, AccountsRoot: root}
	th := throttle.Load(root)
	list := []*accountData{{Token: "t", NeedsFetch: true}}
	list[0].Acct.Name = "acct"

	updates := launchFetches(context.Background(), cfg, list, th)
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
	list, _ := prepareWith(cfg, accounts.Discover(cfg), throttle.Load(cfg.AccountsRoot), sources{
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
	if list[0].NeedsFetch {
		t.Error("must not fetch without a usable credential")
	}
}

// A 200 proves the account's request budget recovered, whatever its body
// turns out to say. Withholding the strike reset until rows parse makes a
// later refusal escalate as though refusals had been consecutive.
func TestAnyHTTP200ClearsRefusalStrikes(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli())
	for _, body := range []string{`{}`, `nonsense`} {
		th := throttle.Load(t.TempDir())
		th.NoteRefused("a", now)
		th.NoteRefused("a", now)
		d := &accountData{}
		d.Acct.Name = "a"
		resolve(d, usage.Result{StatusCode: 200, Body: []byte(body)}, th, now)

		th.NoteRefused("a", now)
		if got := th.NextEligible("a"); !got.Equal(now.Add(throttle.CooldownBase)) {
			t.Errorf("body %q: strikes survived a 200; next eligible %v after base %v",
				body, got.Sub(now), throttle.CooldownBase)
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
	resolve(d, usage.Result{StatusCode: 200, Body: []byte(`{}`)}, throttle.Load(t.TempDir()), now)

	if d.View.Obs == stale {
		t.Fatal("22h-old bars still displayed after the endpoint reported no limits")
	}
	if d.View.Obs == nil || len(d.View.Obs.Rows) != 0 || d.View.Obs.ObservedAt != now.Unix() {
		t.Errorf("zero-row observation not recorded: %+v", d.View.Obs)
	}
}
