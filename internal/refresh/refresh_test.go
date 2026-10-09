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
	"github.com/qiushiyan/headroom/internal/usagelog"
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
			// A reading lands in the usage log beside the ledger; a request
			// that brought none leaves no line.
			logged, _, err := usagelog.Read(root)
			switch {
			case err != nil:
				t.Fatal(err)
			case tc.rows < 0 && len(logged) != 0:
				t.Errorf("a request with no reading logged %+v", logged)
			case tc.rows >= 0 && (len(logged) != 1 || len(logged[0].Rows) != tc.rows || logged[0].Key != key.ID() || logged[0].Home != st.Home()):
				t.Errorf("usage log = %+v", logged)
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

// A reading goes to the usage log beside the subscription ledger — another
// home's root when this home spends against its ledger — carrying what the
// response said and which home asked. A log that cannot be written costs the
// line and nothing else: the request completes as it would have.
func TestAReadingIsLoggedBesideTheLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limits":[{"kind":"session","percent":42,"resets_at":"2026-10-09T20:50:00Z"}]}`))
	}))
	defer srv.Close()
	own, ledger := t.TempDir(), t.TempDir()
	st := state.Open(config.Scope{AccountsRoot: own, LedgerRoot: ledger})
	for r := range Start(context.Background(), st, []*Candidate{requestCandidate(t, srv.URL, "a", "secret")}, nil) {
		if r.Attempt.State != accountstate.AttemptOK || r.StoreErr != nil {
			t.Fatalf("result = %+v", r)
		}
	}
	if mine, _, _ := usagelog.Read(own); len(mine) != 0 {
		t.Errorf("logged in the home's own root: %+v", mine)
	}
	logged, _, err := usagelog.Read(ledger)
	if err != nil || len(logged) != 1 {
		t.Fatalf("ledger root's log: %+v %v", logged, err)
	}
	l := logged[0]
	if l.Home != filepath.Clean(own) || l.Name != "a" || l.Key != "dir:a" || len(l.Rows) != 1 ||
		l.Rows[0].Percent != 42 || l.Rows[0].Kind != "session" || l.Rows[0].ResetsAt == nil || *l.Rows[0].ResetsAt != "2026-10-09T20:50:00Z" {
		t.Errorf("line = %+v", l)
	}

	broken := t.TempDir()
	if err := os.Mkdir(usagelog.Path(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	st = state.Open(config.Scope{AccountsRoot: broken})
	for r := range Start(context.Background(), st, []*Candidate{requestCandidate(t, srv.URL, "a", "secret")}, nil) {
		if r.Attempt.State != accountstate.AttemptOK || r.StoreErr != nil || r.Observation == nil {
			t.Errorf("with an unwritable log: %+v", r)
		}
	}
	if _, ok := st.Load().Observation(state.Key{Name: "a"}, time.Now()); !ok {
		t.Error("an unwritable log cost the stored reading")
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
	// The reading measured the account all the same: it is in the usage log.
	if logged, _, _ := usagelog.Read(root); len(logged) != 1 || logged[0].Rows[0].Percent != 42 {
		t.Errorf("usage log after a failed completion = %+v", logged)
	}
}

// A round already cancelled has nothing that could leave, so it claims
// nothing: a claim then would only silence the account for a spacing.
func TestACancelledRoundClaimsNothing(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked++ }))
	defer srv.Close()
	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range Start(ctx, st, []*Candidate{requestCandidate(t, srv.URL, "a", "t")}, nil) {
	}
	if asked != 0 {
		t.Errorf("a cancelled round asked %d times", asked)
	}
	if next := st.Load().NextEligible(state.Key{Name: "a"}, time.Now()); !next.IsZero() {
		t.Errorf("a cancelled round claimed: next eligible %v", next)
	}
}
