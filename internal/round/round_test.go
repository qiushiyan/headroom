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
