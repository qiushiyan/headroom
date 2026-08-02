package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
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
func TestFetchClaimsBudgetBeforeRequesting(t *testing.T) {
	root := t.TempDir()
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
		w.Write([]byte(`{"limits":[{"kind":"session","percent":1}]}`))
	}))
	defer srv.Close()

	cfg := config.Config{UsageURL: srv.URL, AccountsRoot: root}
	th := throttle.Load(root)
	list := []*accountData{{Token: "t", NeedsFetch: true}}
	list[0].Acct.Name = "acct"

	updates := launchFetches(context.Background(), cfg, list, th)
	// The request is still in flight; a concurrent process reading the store
	// from disk must already see the account as spent.
	if other := throttle.Load(root); other.Eligible("acct", time.Now()) {
		t.Error("account still eligible while its request is in flight")
	}
	close(released)
	for range updates {
	}
}
