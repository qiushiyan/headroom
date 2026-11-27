package accountstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// peerHome is a home whose primary is uuid-1, with a reading of its own of
// pct percent stored ownAge ago, and mini2's reading of peerPct percent
// pulled, made peerAge ago.
func peerHome(t *testing.T, peers []string, ownAge, peerAge time.Duration) (Snapshot, time.Time) {
	t.Helper()
	body := func(pct int) []byte {
		return fmt.Appendf(nil, `{"limits":[{"kind":"weekly_all","group":"weekly","percent":%d}]}`, pct)
	}
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	cfg.Peers = peers
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"}}`)
	now := time.Now()
	store := state.Open(cfg)
	key := state.Key{UUID: "uuid-1", Name: "primary"}
	at := now.Add(-ownAge)
	dec, _ := store.Claim([]state.Key{key}, at)
	if _, err := store.Complete(key, dec[0].Generation, state.OutcomeStored, body(40), at); err != nil {
		t.Fatal(err)
	}
	store.ClaimPull("mini2", now)
	store.RecordPull("mini2", map[string]state.Observation{key.ID(): {FetchedAtMS: now.Add(-peerAge).UnixMilli(), Body: body(60)}}, "", now)
	return Read(cfg, store, now), now
}

// The newest reading wins whichever machine made it, and a machine's
// readings are read only while the scope names it: unset, a home reads its
// own as it always did.
func TestTheNewestReadingWinsAcrossMachines(t *testing.T) {
	snap, _ := peerHome(t, []string{"mini2"}, 12*time.Hour, 2*time.Minute)
	v := snap.Accounts[0].View
	if v.Obs.Source != SourcePeer || v.Obs.Peer != "mini2" || v.Obs.Rows[0].Percent != 60 {
		t.Errorf("chose %d%% from %v %q; want mini2's 60%%", v.Obs.Rows[0].Percent, v.Obs.Source, v.Obs.Peer)
	}

	snap, _ = peerHome(t, []string{"mini2"}, time.Minute, 2*time.Minute)
	if v := snap.Accounts[0].View; v.Obs.Source != SourceStore || v.Obs.Rows[0].Percent != 40 {
		t.Errorf("an older reading from mini2 beat this machine's own: %d%% from %v", v.Obs.Rows[0].Percent, v.Obs.Source)
	}

	snap, _ = peerHome(t, nil, 12*time.Hour, 2*time.Minute)
	if v := snap.Accounts[0].View; v.Obs.Source != SourceStore {
		t.Errorf("a home naming no peer read a pulled reading: source %v", v.Obs.Source)
	}
}

// A pulled reading is current for as long as placement takes any reading as
// current: the machine that made it asks as often as its own surfaces run,
// and this one cannot ask sooner itself.
func TestAPulledReadingIsFreshWhilePlacementTrustsIt(t *testing.T) {
	snap, now := peerHome(t, []string{"mini2"}, 12*time.Hour, placement.StaleAfter-time.Minute)
	v := snap.Accounts[0].View
	if !v.Fresh(now.Unix()) {
		t.Error("a reading pulled from mini2, made inside placement's window, reads as stale")
	}
	snap, now = peerHome(t, []string{"mini2"}, 12*time.Hour, placement.StaleAfter+time.Minute)
	if v := snap.Accounts[0].View; v.Fresh(now.Unix()) {
		t.Error("a reading older than placement's window reads as fresh")
	}
}

// What this machine hands another is its own: a reading it pulled never
// travels on, so nothing comes back under a second machine's name.
func TestOwnReadingsAreNeverPulledOnes(t *testing.T) {
	snap, _ := peerHome(t, []string{"mini2"}, 12*time.Hour, time.Minute)
	own := OwnReadings(snap.Set, snap.Store, time.Now())
	if len(own) != 1 || own[0].Key.UUID != "uuid-1" {
		t.Fatalf("own readings = %+v, want the primary's", own)
	}
	var got bytes.Buffer
	if err := json.Compact(&got, own[0].Obs.Body); err != nil || got.String() != `{"limits":[{"kind":"weekly_all","group":"weekly","percent":40}]}` {
		t.Errorf("handed on %s, want this machine's own 40%% reading", own[0].Obs.Body)
	}
}
