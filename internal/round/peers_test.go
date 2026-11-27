package round

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/peers"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

func weekly(pct int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":"2099-01-01T00:00:00Z"}]}`, pct))
}

// answer is another machine's readings document holding one reading per id,
// each with the percent given, made at.
func answer(at time.Time, percents map[string]int) []byte {
	doc := peers.Document{Schema: peers.Schema, Vendor: string(config.Claude), Readings: []peers.Reading{}}
	for id, p := range percents {
		doc.Readings = append(doc.Readings, peers.Reading{Account: "uuid:" + id, Name: id, FetchedAtMS: at.UnixMilli(), Body: weekly(p)})
	}
	b, _ := json.Marshal(doc)
	return b
}

func pairedScope(t *testing.T) config.Scope {
	return config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
}

// peerRound is a round over accounts a1 and b1 of scope, each holding what
// the store holds for it, its aged-out token its attempt, pulling through ask.
func peerRound(t *testing.T, scope config.Scope, ask peers.Asker) *Round {
	t.Helper()
	st := state.Open(scope)
	r := &Round{Scope: scope, Store: st, pull: ask}
	snap, now := st.Load(), time.Now()
	for _, id := range []string{"a1", "b1"} {
		acct := accounts.Account{Name: id + "@example.com", Scope: scope, AccountID: id, Readable: true}
		key := state.KeyFor(acct)
		r.rows = append(r.rows, &row{Acct: acct, Key: key,
			View: accountstate.Facts{Obs: accountstate.Select(snap, key, acct, now), Attempt: accountstate.Attempt{State: accountstate.AttemptTokenStale}}})
	}
	return r
}

// store records body as this machine's own fetch of id at at.
func store(t *testing.T, st *state.Store, id string, body []byte, at time.Time) {
	t.Helper()
	k := state.Key{UUID: id}
	dec, err := st.Claim([]state.Key{k}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Complete(k, dec[0].Generation, state.OutcomeStored, body, at); err != nil {
		t.Fatal(err)
	}
}

// pulled records body as mini2's reading of id made at.
func pulled(t *testing.T, st *state.Store, id string, body []byte, at time.Time) {
	t.Helper()
	c, err := st.ClaimPull("mini2", time.Now())
	if err != nil || !c.Permit {
		t.Fatalf("claim: %+v %v", c, err)
	}
	if err := st.RecordPull("mini2", c.Generation, map[string]state.Observation{"uuid:" + id: {FetchedAtMS: at.UnixMilli(), Body: body}}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// After a pull the board shows what the next launch reads from disk: every
// account's figures are chosen again, by the assembly's rule, over the store
// the pull wrote. A pull says nothing about this machine's request, whose
// attempt stands as it was.
func TestAPullLeavesTheBoardOnTheStoresChoice(t *testing.T) {
	now := time.Now()
	scope := pairedScope(t)
	st := state.Open(scope)
	store(t, st, "a1", weekly(10), now.Add(-time.Hour))
	store(t, st, "b1", weekly(20), now.Add(-time.Second))
	ask := func(_ context.Context, peer string, v config.Vendor) ([]byte, error) {
		if peer != "mini2" || v != config.Claude {
			t.Errorf("asked %s for %s", peer, v)
		}
		return answer(now.Add(-30*time.Second), map[string]int{"a1": 80, "b1": 90}), nil
	}
	r := peerRound(t, scope, ask)
	r.Complete(context.Background())

	disk := st.Load()
	for _, d := range r.rows {
		want := accountstate.Select(disk, d.Key, d.Acct, time.Now())
		if d.View.Obs == nil || want == nil || d.View.Obs.ObservedAt != want.ObservedAt || d.View.Obs.Source != want.Source {
			t.Errorf("%s: board shows %+v, disk chooses %+v", d.Acct.Name, d.View.Obs, want)
		}
		if d.View.Attempt.State != accountstate.AttemptTokenStale {
			t.Errorf("%s: attempt became %v; a pull says nothing about this machine's request", d.Acct.Name, d.View.Attempt.State)
		}
	}
	if a := r.rows[0].View.Obs; a.Source != accountstate.SourcePeer || a.Peer != "mini2" || a.Rows[0].Percent != 80 {
		t.Errorf("a1 = %+v, want mini2's newer 80%%", a)
	}
	if b := r.rows[1].View.Obs; b.Source != accountstate.SourceStore || b.Rows[0].Percent != 20 {
		t.Errorf("b1 = %+v, want this machine's own newer 20%%", b)
	}
}

// Whichever arrives first — this process's live answer or a pull's choice —
// the board comes to the same figures, the ones the store would choose: a
// live answer stands over an older pulled reading, and a pulled reading
// stamped by a clock ahead of this one stands over the live answer, on the
// board as on disk. A live answer the store never recorded survives a pull.
func TestEveryOrderComesToOneAnswer(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		peerAt    time.Time
		wantPeer  bool
		liveSaved bool
	}{
		{"live newer than the pulled reading", now.Add(-30 * time.Second), false, true},
		{"a pulled reading from a clock ahead", now.Add(time.Minute), true, true},
		{"a live answer the store never recorded", now.Add(-30 * time.Second), false, false},
	} {
		for _, liveFirst := range []bool{true, false} {
			scope := pairedScope(t)
			st := state.Open(scope)
			pulled(t, st, "a1", weekly(80), c.peerAt)
			if c.liveSaved {
				store(t, st, "a1", weekly(5), now)
			}
			r := peerRound(t, scope, nil)
			liveObs := &accountstate.Observation{ObservedAt: now.Unix(), Source: accountstate.SourceLive}
			src := make(chan refresh.Result, 1)
			src <- refresh.Result{Index: 0, Observation: liveObs}
			close(src)
			snap := st.Load()
			reconcile := Result{of: r, selected: []*accountstate.Observation{
				accountstate.Select(snap, r.rows[0].Key, r.rows[0].Acct, now), accountstate.Select(snap, r.rows[1].Key, r.rows[1].Acct, now)}}
			live := <-r.Feed(src)
			if liveFirst {
				r.Apply(live)
				r.Apply(reconcile)
			} else {
				r.Apply(reconcile)
				r.Apply(live)
			}
			got := r.rows[0].View.Obs
			if isPeer := got != nil && got.Source == accountstate.SourcePeer; isPeer != c.wantPeer {
				t.Errorf("%s, live first %v: board shows %+v", c.name, liveFirst, got)
			}
		}
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
	alone := pairedScope(t)
	alone.Peers = nil
	peerRound(t, alone, ask).Complete(context.Background())
	if asked.Load() != 0 {
		t.Fatalf("a scope naming no peer asked %d times", asked.Load())
	}

	paired := pairedScope(t)
	peerRound(t, paired, ask).Complete(context.Background())
	peerRound(t, paired, ask).Complete(context.Background())
	if asked.Load() != 1 {
		t.Errorf("two rounds a moment apart asked %d times, want once", asked.Load())
	}
}

// A machine that cannot answer costs the board nothing: the failure is
// recorded beside the last answer. This machine failing to record a pull is
// its own state's problem, and the round says so.
func TestPullFailuresAreSaidWhereTheyBelong(t *testing.T) {
	scope := pairedScope(t)
	ask := func(context.Context, string, config.Vendor) ([]byte, error) {
		return nil, fmt.Errorf("ssh could not reach it: timed out")
	}
	r := peerRound(t, scope, ask)
	r.Complete(context.Background())
	if r.rows[0].View.Obs != nil || len(r.Problems) != 0 {
		t.Errorf("a machine not answering produced figures %+v or problems %v", r.rows[0].View.Obs, r.Problems)
	}
	if st, _ := r.Store.Load().PeerStatus("mini2"); st.Error != "ssh could not reach it: timed out" {
		t.Errorf("status = %+v, want the failure recorded", st)
	}

	broken := pairedScope(t)
	broken.AccountsRoot = filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(broken.AccountsRoot, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r = peerRound(t, broken, ask)
	r.Complete(context.Background())
	if len(r.Problems) != 1 || r.Problems[0].Section != "peers" || !strings.Contains(r.Problems[0].Detail, "mini2") {
		t.Errorf("problems = %+v, want this machine's failure to claim the pull", r.Problems)
	}
}
