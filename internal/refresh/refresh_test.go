package refresh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
)

func requestCandidate(t *testing.T, url, name, token string) *Candidate {
	t.Helper()
	c, status := Prepare(accounts.Account{Scope: config.Scope{UsageURL: url}, Name: name, Readable: true}, creds.Blob{Token: token, ExpiresState: tag.None}, true, time.Now())
	if c == nil || status != accountstate.AttemptPending {
		t.Fatalf("candidate: %v %v", c, status)
	}
	return c
}

func TestRequestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want accountstate.AttemptState
		rows int
	}{
		{"rows", 200, `{"limits":[{"kind":"session","percent":42}]}`, accountstate.AttemptOK, 1},
		{"empty", 200, `{"limits":[]}`, accountstate.AttemptNoLimits, 0},
		{"malformed", 200, `broken`, accountstate.AttemptUnparseable, -1},
		{"refused", 429, ``, accountstate.AttemptRefused, -1},
		{"unauthorized", 401, ``, accountstate.AttemptHTTP, -1},
		{"transport", 0, ``, accountstate.AttemptTransport, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			st := state.Open(config.Scope{AccountsRoot: root})
			key := state.Key{Name: "a"}
			before := time.Now().Truncate(time.Millisecond)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer secret" {
					t.Errorf("authorization=%q", got)
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if tc.code == 0 {
				srv.Close()
			}
			results := 0
			for r := range Start(context.Background(), st, []*Candidate{requestCandidate(t, srv.URL, "a", "secret")}, nil) {
				results++
				if r.Attempt.State != tc.want || r.Attempt.HTTPCode != tc.code || r.StoreErr != nil {
					t.Fatalf("result=%+v", r)
				}
				stored, ok := st.Load().Observation(key, time.Now())
				if tc.rows >= 0 {
					if r.Observation == nil || len(r.Observation.Rows) != tc.rows {
						t.Fatalf("received observation=%+v", r.Observation)
					}
					if r.Observation.Source != accountstate.SourceLive || (tc.rows == 1 && r.Observation.Rows[0].Percent != 42) {
						t.Errorf("received facts=%+v", r.Observation)
					}
					if r.Observation.ObservedAt < before.Unix() || r.Observation.ObservedAt > time.Now().Unix() {
						t.Errorf("arrival timestamp=%d", r.Observation.ObservedAt)
					}
					var compact bytes.Buffer
					_ = json.Compact(&compact, stored.Body)
					if !ok || compact.String() != tc.body || stored.FetchedAtMS/1000 != r.Observation.ObservedAt {
						t.Errorf("stored=%+v, ok=%v", stored, ok)
					}
				} else if r.Observation != nil || ok {
					t.Fatalf("failed request replaced observation: %+v %+v", r, stored)
				}
				if tc.code == 401 && r.TokenAfter401 != TokenUnknown {
					t.Error("unsampled credentials asserted unchanged")
				}
			}
			if results != 1 {
				t.Fatalf("results=%d", results)
			}
			next := st.Load().NextEligible(key, time.Now())
			spacing := config.DefaultSpacing
			if tc.code == 429 {
				spacing = state.CooldownBase
			}
			if next.Before(before.Add(spacing)) || next.After(time.Now().Add(spacing)) {
				t.Errorf("deadline=%v, spacing=%v", next, spacing)
			}
		})
	}
}

func TestAnyHTTP200ClearsRefusalStrikes(t *testing.T) {
	for _, body := range []string{`{"limits":[]}`, `nonsense`} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			key := state.Key{Name: "a"}
			now := time.Now()
			// An eligible ledger with earlier refusals: no fake clock or manual
			// completion path stands between this response and the next refusal.
			seed := fmt.Sprintf(`{"version":1,"accounts":{"dir:a":{"request":{"last_attempt_ms":%d,"next_eligible_ms":%d,"strikes":2}}}}`, now.Add(-time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli())
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(seed), 0600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer srv.Close()
			st := state.Open(config.Scope{AccountsRoot: root})
			for r := range Start(context.Background(), st, []*Candidate{requestCandidate(t, srv.URL, "a", "token")}, nil) {
				if r.StoreErr != nil {
					t.Fatal(r.StoreErr)
				}
			}
			at := time.Now().Add(time.Hour).Truncate(time.Millisecond)
			dec, err := st.Claim([]state.Key{key}, at)
			if err != nil || !dec[0].Permit {
				t.Fatalf("claim=%+v %v", dec, err)
			}
			next, err := st.Complete(key, dec[0].Generation, state.OutcomeRefused, nil, at)
			if err != nil || next.Sub(at) != state.CooldownBase {
				t.Fatalf("200 did not reset refusal escalation: %v %v", next.Sub(at), err)
			}
		})
	}
}

func TestFetchClaimsBudgetBeforeRequesting(t *testing.T) {
	root := t.TempDir()
	released := make(chan struct{})
	var eligibleAtRequestTime bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What a separate process would see, reading the store off disk right
		// as this request arrives.
		now := time.Now()
		eligibleAtRequestTime = !state.Open(config.Scope{AccountsRoot: root}).Load().
			NextEligible(state.Key{Name: "acct"}, now).After(now)
		<-released
		w.Write([]byte(`{"limits":[{"kind":"session","percent":1}]}`))
	}))
	defer srv.Close()

	st := state.Open(config.Scope{AccountsRoot: root})
	updates := Start(context.Background(), st, []*Candidate{requestCandidate(t, srv.URL, "acct", "t")}, nil)

	close(released)
	for range updates {
	}
	if eligibleAtRequestTime {
		t.Error("the claim was not on disk when the request was served: " +
			"a concurrent process would have spent the same account's budget")
	}
}

func TestUnauthorizedSamplesCredentialsOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	for _, tc := range []struct {
		raw  string
		want TokenEvidence
	}{
		{`{"claudeAiOauth":{"accessToken":"new"}}`, TokenChanged},
		{`{"claudeAiOauth":{"accessToken":"old"}}`, TokenUnchanged},
		{`unreadable`, TokenUnknown},
	} {
		reads := 0
		for r := range Start(context.Background(), state.Open(config.Scope{AccountsRoot: t.TempDir()}), []*Candidate{requestCandidate(t, srv.URL, "a", "old")}, func(string) (string, bool) { reads++; blob, ok := creds.Parse(tc.raw); return blob.Token, ok }) {
			if r.TokenAfter401 != tc.want || r.Attempt.HTTPCode != 401 {
				t.Fatalf("evidence: %+v", r)
			}
		}
		if reads != 1 {
			t.Fatalf("credential samples = %d", reads)
		}
	}
}

func TestReceivedObservationSurvivesCompletionFailure(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A different binary replaces the state after authorization, before completion.
		if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":999}`), 0600); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"limits":[{"kind":"session","percent":42}]}`))
	}))
	defer srv.Close()
	for r := range Start(context.Background(), state.Open(config.Scope{AccountsRoot: root}), []*Candidate{requestCandidate(t, srv.URL, "a", "t")}, nil) {
		if !errors.Is(r.StoreErr, state.ErrReadOnly) || r.Observation == nil || r.Observation.Rows[0].Percent != 42 {
			t.Fatalf("lost response or persistence evidence: %+v", r)
		}
	}
}
