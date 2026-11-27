package round

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/peers"
	"github.com/qiushiyan/headroom/internal/state"
)

// answer is another machine's readings document holding one reading per id,
// each with the percent given, fetched at.
func answer(at time.Time, percents map[string]int) []byte {
	doc := peers.Document{Schema: peers.Schema, Vendor: string(config.Claude)}
	for id, p := range percents {
		doc.Readings = append(doc.Readings, peers.Reading{Account: "uuid:" + id, Name: id, FetchedAtMS: at.UnixMilli(),
			Body: json.RawMessage(fmt.Sprintf(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":"2099-01-01T00:00:00Z"}]}`, p))})
	}
	b, _ := json.Marshal(doc)
	return b
}

func peerRound(t *testing.T, scope config.Scope, ask peers.Asker, own map[string]*accountstate.Observation) *Round {
	t.Helper()
	r := &Round{Scope: scope, Store: state.Open(scope), pull: ask}
	for _, id := range []string{"a1", "b1"} {
		acct := accounts.Account{Name: id + "@example.com", Scope: scope, AccountID: id, Readable: true}
		r.rows = append(r.rows, &row{Acct: acct, Key: state.KeyFor(acct),
			View: accountstate.Facts{Obs: own[id], Attempt: accountstate.Attempt{State: accountstate.AttemptTokenStale}}})
	}
	return r
}

// Another machine's reading lands where it is the newest one there is, and
// nowhere else; and it says nothing about this machine's request, whose
// attempt stands as it was.
func TestAPullLandsWhereItIsNewest(t *testing.T) {
	now := time.Now()
	scope := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
	own := map[string]*accountstate.Observation{
		"a1": {ObservedAt: now.Add(-time.Hour).Unix(), Source: accountstate.SourceStore},
		"b1": {ObservedAt: now.Unix(), Source: accountstate.SourceLive},
	}
	ask := func(_ context.Context, peer string, v config.Vendor) ([]byte, error) {
		if peer != "mini2" || v != config.Claude {
			t.Errorf("asked %s for %s", peer, v)
		}
		return answer(now.Add(-30*time.Second), map[string]int{"a1": 80, "b1": 10}), nil
	}
	r := peerRound(t, scope, ask, own)
	r.Complete(context.Background())

	a, b := r.rows[0].View, r.rows[1].View
	if a.Obs == nil || a.Obs.Source != accountstate.SourcePeer || a.Obs.Peer != "mini2" || a.Obs.Rows[0].Percent != 80 {
		t.Errorf("a's figures = %+v, want mini2's newer reading", a.Obs)
	}
	if b.Obs != own["b1"] {
		t.Errorf("b's own newer reading was replaced by %+v", b.Obs)
	}
	if a.Attempt.State != accountstate.AttemptTokenStale {
		t.Errorf("a's attempt became %v; a pull says nothing about this machine's request", a.Attempt.State)
	}

	// And what was pulled is on disk, for a reader that never pulls.
	snap := r.Store.Load()
	if obs := accountstate.PeerObservation(snap, r.rows[0].Key, r.rows[0].Acct, "mini2", now); obs == nil || obs.Rows[0].Percent != 80 {
		t.Errorf("the pulled reading is not in the store: %+v", obs)
	}
}

// Opt-in: a scope naming no peer asks nobody, and a second round inside the
// spacing does not ask the same machine again.
func TestPullsAreOptInAndSpaced(t *testing.T) {
	var asked atomic.Int32
	ask := func(context.Context, string, config.Vendor) ([]byte, error) {
		asked.Add(1)
		return answer(time.Now(), map[string]int{"a1": 1}), nil
	}
	alone := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing}
	peerRound(t, alone, ask, nil).Complete(context.Background())
	if asked.Load() != 0 {
		t.Fatalf("a scope naming no peer asked %d times", asked.Load())
	}

	paired := alone
	paired.AccountsRoot, paired.Peers = t.TempDir(), []string{"mini2"}
	peerRound(t, paired, ask, nil).Complete(context.Background())
	peerRound(t, paired, ask, nil).Complete(context.Background())
	if asked.Load() != 1 {
		t.Errorf("two rounds a moment apart asked %d times, want once", asked.Load())
	}
}

// A machine that cannot answer costs its readings nothing and the round
// nothing more: the failure is recorded beside the last answer.
func TestAFailedPullIsRecordedNotShown(t *testing.T) {
	scope := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
	ask := func(context.Context, string, config.Vendor) ([]byte, error) {
		return nil, fmt.Errorf("ssh could not reach it: timed out")
	}
	r := peerRound(t, scope, ask, nil)
	r.Complete(context.Background())
	if r.rows[0].View.Obs != nil {
		t.Errorf("a failed pull produced figures: %+v", r.rows[0].View.Obs)
	}
	if st, _ := r.Store.Load().PeerStatus("mini2"); st.Error != "ssh could not reach it: timed out" {
		t.Errorf("status = %+v, want the failure recorded", st)
	}
}
