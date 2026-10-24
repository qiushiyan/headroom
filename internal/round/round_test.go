package round

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Regression test for the picker data race: the consumer applies each result
// and then — like the picker's draw — reads every account's view, while other
// fetches are still in flight. Fetch goroutines writing views themselves
// made this fail under -race; Fetch must keep views single-writer.
func TestFetchKeepsViewsSingleWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "slow") {
			time.Sleep(50 * time.Millisecond)
		}
		w.Write([]byte(`{"limits":[{"kind":"session","percent":5,"resets_at":"2026-08-02T15:00:00Z"}]}`))
	}))
	defer srv.Close()

	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	r := &Round{Store: st, rows: []*row{
		{request: candidate(srv.URL, "fast", "fast"),
			View: accountstate.Facts{Attempt: accountstate.Attempt{State: accountstate.AttemptPending}}},
		{request: candidate(srv.URL, "slow", "slow"),
			View: accountstate.Facts{Attempt: accountstate.Attempt{State: accountstate.AttemptPending}}},
	}}
	r.rows[0].Acct.Name, r.rows[1].Acct.Name = "fast", "slow"
	for u := range r.Fetch(context.Background()) {
		r.Apply(u)
		for _, d := range r.rows {
			_ = d.View.Attempt.State
			if d.View.Obs != nil {
				_ = len(d.View.Obs.Rows)
			}
		}
	}
	for i, d := range r.rows {
		if d.View.Attempt.State != accountstate.AttemptOK {
			t.Errorf("account %d not resolved: attempt %v", i, d.View.Attempt.State)
		}
	}
}

// The board reads each credential once, in prepare. Sampling the token again
// after a 401 is check's diagnostic, and the board makes no such extra read —
// no `security` spawn per rejected account for evidence it never shows.
func TestARoundLeaves401CredentialSamplingToChecker(t *testing.T) {
	bin := t.TempDir()
	sampled := filepath.Join(t.TempDir(), "sampled")
	t.Setenv("PATH", bin)
	t.Setenv("HEADROOM_TEST_CREDENTIAL_SAMPLE", sampled)
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\necho sampled > \"$HEADROOM_TEST_CREDENTIAL_SAMPLE\"\nexit 44\n"), 0755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	rd := &Round{Store: state.Open(config.Scope{AccountsRoot: t.TempDir()}), rows: []*row{{request: candidate(srv.URL, "a", "token")}}}
	for r := range rd.Fetch(context.Background()) {
		if r.Attempt.HTTPCode != 401 || r.TokenAfter401 != refresh.TokenUnknown {
			t.Fatalf("board's evidence=%+v", r)
		}
	}
	if _, err := os.Stat(sampled); !os.IsNotExist(err) {
		t.Fatal("board sampled credentials for diagnostic evidence it does not consume")
	}
}

func candidate(url, name, token string) *refresh.Candidate {
	c, _ := refresh.Prepare(accounts.Account{Scope: config.Scope{UsageURL: url}, Name: name, Readable: true}, creds.Blob{Token: token, ExpiresAtMS: time.Now().Add(time.Hour).UnixMilli()}, true, time.Now())
	return c
}

// A fetch that lands is an answer: a 200 with limit windows and a 200 with
// none both leave the account current.
func TestASuccessfulFetchIsAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       accountstate.AttemptState
	}{
		{"rows", `{"limits":[{"kind":"session","percent":42}]}`, accountstate.AttemptOK},
		{"empty", `{"limits":[]}`, accountstate.AttemptNoLimits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			r := &Round{Store: state.Open(config.Scope{AccountsRoot: t.TempDir()}), rows: []*row{{request: candidate(srv.URL, "a", "token")}}}
			r.Complete(context.Background())
			if got := r.Accounts()[0].View.Attempt; got.State != tc.want {
				t.Errorf("attempt %+v, want %v", got, tc.want)
			}
		})
	}
}

// Apply writes one view and nothing else — the store has already been told,
// by the goroutine that fetched — and it writes each axis on its own. The
// headline regression: a request that failed says nothing about the account,
// so it annotates what is known rather than erasing it. Before the three-axis
// model, a 429 replaced the rows and the whole board read as broken. A
// reading, zero rows included ("this account reports no limits" is newer
// truth than any cached bars), replaces the figures with its own stamp:
// rows without their time are how carried-over data passed itself off as
// current. And a bookkeeping failure rides beside the endpoint's verdict
// without replacing it.
func TestApplyWritesEachAxisOnItsOwn(t *testing.T) {
	now := time.Now()
	at := now.Add(-5 * time.Second).Unix()
	prior := &accountstate.Observation{Rows: []usage.Row{{Label: "5h session", Percent: 42}},
		ObservedAt: now.Add(-22 * time.Hour).Unix(), Source: accountstate.SourceCache}
	reading := &accountstate.Observation{Rows: []usage.Row{{Percent: 5}, {Percent: 9}}, ObservedAt: at, Source: accountstate.SourceLive}
	noLimits := &accountstate.Observation{ObservedAt: at, Source: accountstate.SourceLive}

	for _, c := range []struct {
		name     string
		result   refresh.Result
		obs      *accountstate.Observation // the view's afterwards
		storeErr string
	}{
		{"a reading replaces the figures, stamp intact",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptOK}, Observation: reading}, reading, ""},
		{"no limits replaces old bars",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptNoLimits}, Observation: noLimits}, noLimits, ""},
		{"a 429 annotates and never erases",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, HTTPCode: 429}}, prior, ""},
		{"a transport failure annotates and never erases",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptTransport}}, prior, ""},
		{"a store error rides beside a reading",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptOK}, Observation: reading, StoreErr: state.ErrReadOnly}, reading, state.ErrReadOnly.Error()},
		{"a store error rides beside a refusal",
			refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, HTTPCode: 429}, StoreErr: state.ErrReadOnly}, prior, state.ErrReadOnly.Error()},
	} {
		r := Of(accounts.Set{}, nil, accounts.Routing{}, []accountstate.Account{{View: accountstate.Facts{Health: accountstate.HealthOK, Obs: prior}}})
		r.Apply(Result{Result: c.result, of: r})
		d := r.Accounts()[0]
		if d.View.Obs != c.obs {
			t.Errorf("%s: observation %+v, want %+v", c.name, d.View.Obs, c.obs)
		}
		if c.obs == reading && d.View.Obs.ObservedAt != at {
			t.Errorf("%s: the reading was restamped: %d, want %d", c.name, d.View.Obs.ObservedAt, at)
		}
		if d.View.Attempt.State != c.result.Attempt.State || d.View.Attempt.StoreError != c.storeErr {
			t.Errorf("%s: attempt %+v, want state %v beside store error %q", c.name, d.View.Attempt, c.result.Attempt.State, c.storeErr)
		}
		if d.View.Health != accountstate.HealthOK {
			t.Errorf("%s: a request's outcome changed health: %v", c.name, d.View.Health)
		}
	}
}

// A result lands on the round it answers and nowhere else: delivered to
// another round, its index would name that round's account at the same
// position, which is somebody else's.
func TestApplyIgnoresAnotherRoundsResult(t *testing.T) {
	facts := func() []accountstate.Account {
		return []accountstate.Account{{View: accountstate.Facts{Label: "a@x.com"}}, {View: accountstate.Facts{Label: "b@x.com"}}}
	}
	ours, theirs := Of(accounts.Set{}, nil, accounts.Routing{}, facts()), Of(accounts.Set{}, nil, accounts.Routing{}, facts())
	src := make(chan refresh.Result, 1)
	reading := &accountstate.Observation{Rows: []usage.Row{{Percent: 61}}, ObservedAt: time.Now().Unix(), Source: accountstate.SourceLive}
	src <- refresh.Result{Index: 1, Attempt: accountstate.Attempt{State: accountstate.AttemptOK}, Observation: reading}
	close(src)

	for res := range theirs.Feed(src) {
		ours.Apply(res)
		if got := ours.Accounts()[1].View; got.Obs != nil || got.Attempt.State != accountstate.AttemptNone {
			t.Errorf("another round's result landed: %+v", got)
		}
		theirs.Apply(res)
	}
	if got := theirs.Accounts()[1].View.Obs; got != reading {
		t.Errorf("its own round did not take the result: %+v", got)
	}
	if got := theirs.Accounts()[0].View.Obs; got != nil {
		t.Errorf("the result landed on another index: %+v", got)
	}
}
