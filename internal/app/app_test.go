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
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// oneRound is what every surface does: prepare, claim, fetch, resolve. A fresh
// store handle each time, because every surface is a fresh process — and the
// bugs below only appear across two rounds, since the first is what leaves the
// ledger in the state the second one reads.
func oneRound(t *testing.T, cfg config.Scope, blobs map[string]string, now time.Time) map[string]accountstate.Account {
	t.Helper()
	r := preparedWith(cfg, state.Open(cfg), now, blobs, nil)
	r.Complete(context.Background())
	return byName(r.Accounts())
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
// quiet for one cooldown rather than bricking. That promise is kept by Claim,
// which decides every account's eligibility under its lock from the very
// ledger that is unreadable — prepare only marks a spendable account pending
// and decides nothing. Two runs is the whole point: the first looks correct
// in isolation.
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
	oneRound(t, cfg, blobs, now.Add(time.Second)) // whose it is unreadable: no key, so no claim this run
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

// The board says a bookkeeping failure beside the endpoint's verdict, in both
// layouts, never instead of it: "rate limited" and "state file unavailable"
// can both apply. This is the only guard of render's state-file clause; it
// belongs beside render's TestProvenanceKeepsTheAxesApart and stays here until
// that suite takes it. (A round keeping the two apart is round's
// TestApplyWritesEachAxisOnItsOwn's; the document's problem entry is
// TestJSONDocument's.)
func TestPersistenceFailureKeepsEndpointVerdict(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		state  accountstate.AttemptState
		phrase string
	}{
		{accountstate.AttemptOK, ""}, {accountstate.AttemptRefused, "rate limited"},
	} {
		v := accountstate.Facts{
			Obs:     &accountstate.Observation{Rows: []usage.Row{{Percent: 42}}, ObservedAt: now.Unix(), Source: accountstate.SourceLive},
			Attempt: accountstate.Attempt{State: tc.state, StoreError: state.ErrReadOnly.Error()},
		}
		for _, layout := range []render.Layout{render.LayoutBlocks, render.LayoutCompact} {
			text := strings.Join(render.NewPalette(false).Board([]accountstate.Facts{v}, now.Unix(), layout, 0).Groups[0], "\n")
			if !strings.Contains(text, "state file unavailable") || !strings.Contains(text, tc.phrase) {
				t.Errorf("missing independent evidence: %s", text)
			}
		}
	}
}

// A refresh that landed answers the manual `r` with "all current" — a 200
// with no limit windows is as much an answer as one with — and the document
// carries no error status for it. (What a 200 lands as is round's
// TestASuccessfulFetchIsAnAnswer.)
func TestSuccessfulRefreshPresentation(t *testing.T) {
	for _, state := range []accountstate.AttemptState{accountstate.AttemptOK, accountstate.AttemptNoLimits} {
		accts := []accountstate.Account{{Acct: accounts.Account{Name: "a"}, View: accountstate.Facts{Attempt: accountstate.Attempt{State: state, HTTPCode: 200}}}}
		ui := page{round: round.Of(accounts.Set{}, nil, accounts.Routing{}, accts)}
		if got := ui.ackString(time.Now()); !strings.Contains(got, "all current") {
			t.Errorf("%v: ack=%q", state, got)
		}
		data, err := jsonDocument(claudeBoard(accts, ""), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"http_status"`) {
			t.Errorf("%v: successful attempt gained an error status: %s", state, data)
		}
	}
}
