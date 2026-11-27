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
	"github.com/qiushiyan/headroom/internal/usage"
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

// answering is a start that answers a1's request with pct at at, recorded in
// the store first as refresh.Start records it — or, when recorded is false,
// as an answer whose record failed.
func answering(t *testing.T, pct int, at time.Time, recorded bool) func(context.Context, *state.Store, []*refresh.Candidate) <-chan refresh.Result {
	body := weekly(pct)
	reading, err := usage.Parse(config.Claude, body)
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, st *state.Store, _ []*refresh.Candidate) <-chan refresh.Result {
		res := refresh.Result{Index: 0, Attempt: accountstate.Attempt{State: accountstate.AttemptOK},
			Observation: accountstate.FromReading(reading, at.Unix(), accountstate.SourceLive)}
		if recorded {
			k := state.Key{UUID: "a1"}
			dec, err := st.Claim([]state.Key{k}, at.Add(time.Hour))
			if err != nil || !dec[0].Permit {
				t.Errorf("claim: %+v %v", dec, err)
			}
			if _, err := st.Complete(k, dec[0].Generation, state.OutcomeStored, body, at); err != nil {
				t.Error(err)
			}
		} else {
			res.StoreErr = fmt.Errorf("state file is busy")
		}
		src := make(chan refresh.Result, 1)
		src <- res
		close(src)
		return src
	}
}

// onDisk reports whether every row shows what the store now chooses for it.
func onDisk(t *testing.T, r *Round) {
	t.Helper()
	snap, now := r.Store.Load(), time.Now()
	for _, d := range r.rows {
		want := accountstate.Select(snap, d.Key, d.Acct, now)
		if (d.View.Obs == nil) != (want == nil) || (want != nil && (d.View.Obs.ObservedAt != want.ObservedAt || d.View.Obs.Peer != want.Peer || d.View.Obs.Rows[0].Percent != want.Rows[0].Percent)) {
			t.Errorf("%s: board shows %+v, the store chooses %+v", d.Acct.Name, d.View.Obs, want)
		}
	}
}

// However this machine's answers and two machines' pulls interleave, the
// board ends on what the store chooses: a newer pulled reading over an older
// own one, this machine's newer answer over a pulled one, a pulled reading
// stamped by a clock ahead over this machine's answer — on the board as on
// disk.
func TestARoundThatPullsEndsOnTheStoresChoice(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name     string
		macAt    time.Time
		wantPeer string
	}{
		{"this machine's answer is newest", now.Add(-30 * time.Second), ""},
		{"a pulled reading stamped by a clock ahead", now.Add(time.Minute), "mac"},
	} {
		scope := pairedScope(t)
		scope.Peers = []string{"mini2", "mac"}
		ask := func(_ context.Context, peer string, _ config.Vendor) ([]byte, error) {
			if peer == "mac" {
				return answer(c.macAt, map[string]int{"a1": 91, "b1": 33}), nil
			}
			return answer(now.Add(-time.Minute), map[string]int{"a1": 80, "b1": 30}), nil
		}
		r := peerRound(t, scope, ask)
		r.start = answering(t, 5, now, true)
		r.Complete(context.Background())
		onDisk(t, r)
		if got := r.rows[0].View.Obs; got == nil || got.Peer != c.wantPeer {
			t.Errorf("%s: a1 shows %+v, want peer %q", c.name, got, c.wantPeer)
		}
		if b := r.rows[1].View.Obs; b == nil || b.Peer != "mac" || b.Rows[0].Percent != 33 {
			t.Errorf("%s: b1 shows %+v, want mac's newer 33%%", c.name, b)
		}
	}
}

// An answer of this process's own that the store could not record is still
// the newest reading there is: a pull landing after it does not take it off
// the board.
func TestAnUnrecordedAnswerSurvivesAPull(t *testing.T) {
	now := time.Now()
	scope := pairedScope(t)
	ask := func(context.Context, string, config.Vendor) ([]byte, error) {
		return answer(now.Add(-time.Minute), map[string]int{"a1": 80}), nil
	}
	r := peerRound(t, scope, ask)
	r.start = answering(t, 5, now, false)
	r.Complete(context.Background())
	got := r.rows[0].View.Obs
	if got == nil || got.Source != accountstate.SourceLive || got.ObservedAt != now.Unix() || got.Rows[0].Percent != 5 {
		t.Errorf("a1 shows %+v, want this process's unrecorded 5%% answer", got)
	}
}

// A round that pulls from nobody shows each answer as it lands, as the store
// records it — even over a stored reading stamped ahead by a clock that has
// since stepped back, which the answer replaced on disk.
func TestARoundOnItsOwnShowsWhatItRecorded(t *testing.T) {
	now := time.Now()
	scope := pairedScope(t)
	scope.Peers = nil
	st := state.Open(scope)
	store(t, st, "a1", weekly(10), now.Add(time.Minute))
	r := peerRound(t, scope, nil)
	if r.rows[0].View.Obs == nil || r.rows[0].View.Obs.Rows[0].Percent != 10 {
		t.Fatalf("fixture: a1 = %+v", r.rows[0].View.Obs)
	}
	r.start = answering(t, 88, now, true)
	r.Complete(context.Background())
	onDisk(t, r)
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
