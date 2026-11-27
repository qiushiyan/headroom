package accountstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

func weekly(pct int) []byte {
	return fmt.Appendf(nil, `{"limits":[{"kind":"weekly_all","group":"weekly","percent":%d}]}`, pct)
}

// peerHome is a home whose primary is uuid-1, holding a reading of its own of
// 40% made ownAge ago and mini2's reading of 60% made peerAge ago, read under
// a scope naming peers.
func peerHome(t *testing.T, peers []string, ownAge, peerAge time.Duration) (Snapshot, time.Time) {
	t.Helper()
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"}}`)
	now := time.Now()
	key := state.Key{UUID: "uuid-1", Name: "primary"}

	paired := cfg
	paired.Peers = []string{"mini2"}
	store := state.Open(paired)
	at := now.Add(-ownAge)
	dec, _ := store.Claim([]state.Key{key}, at)
	if _, err := store.Complete(key, dec[0].Generation, state.OutcomeStored, weekly(40), at); err != nil {
		t.Fatal(err)
	}
	c, _ := store.ClaimPull("mini2", now)
	store.RecordPull("mini2", c.Generation, map[string]state.Observation{key.ID(): {FetchedAtMS: now.Add(-peerAge).UnixMilli(), Body: weekly(60)}}, "", now)

	cfg.Peers = peers
	return Read(cfg, state.Open(cfg), now), now
}

// The newest reading wins whichever machine made it, a tie at the second goes
// to this machine's own, and a machine's readings are read only while the
// scope names it: unset, a home reads its own as it always did.
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
	snap, _ = peerHome(t, []string{"mini2"}, time.Minute, time.Minute)
	if v := snap.Accounts[0].View; v.Obs.Source != SourceStore {
		t.Errorf("a tie went to mini2: source %v", v.Obs.Source)
	}
	snap, _ = peerHome(t, nil, 12*time.Hour, 2*time.Minute)
	if v := snap.Accounts[0].View; v.Obs.Source != SourceStore {
		t.Errorf("a home naming no peer read a pulled reading: source %v", v.Obs.Source)
	}
}

// What makes figures current is their age, never who read them: a machine
// that pulls from others holds every reading current for placement's window,
// its own included, and a machine on its own keeps the spacing.
func TestFreshnessIsOneWindowPerConfiguration(t *testing.T) {
	inside, past := placement.StaleAfter-time.Minute, placement.StaleAfter+time.Minute
	for _, c := range []struct {
		name            string
		peers           []string
		ownAge, peerAge time.Duration
		fresh           bool
	}{
		{"pulled, inside the window", []string{"mini2"}, 12 * time.Hour, inside, true},
		{"pulled, past it", []string{"mini2"}, 12 * time.Hour, past, false},
		{"own, inside a paired window", []string{"mini2"}, inside, 12 * time.Hour, true},
		{"own, on a machine alone, past the spacing", nil, 2 * config.DefaultSpacing, 12 * time.Hour, false},
	} {
		snap, now := peerHome(t, c.peers, c.ownAge, c.peerAge)
		if got := snap.Accounts[0].View.Fresh(now.Unix()); got != c.fresh {
			t.Errorf("%s: fresh = %v, want %v", c.name, got, c.fresh)
		}
	}
}

// What this machine hands another is its own — never a reading it pulled, so
// nothing comes back under a second machine's name — and it is the newest of
// its own sources exactly as read: Claude Code's cache when that is newer
// than headroom's fetch, its body and its milliseconds.
func TestOwnReadingsAreThisMachinesNewest(t *testing.T) {
	snap, _ := peerHome(t, []string{"mini2"}, 12*time.Hour, time.Minute)
	own := OwnReadings(snap.Set, snap.Store, time.Now())
	if len(own) != 1 || own[0].Key.UUID != "uuid-1" {
		t.Fatalf("own readings = %+v, want the primary's", own)
	}
	var got bytes.Buffer
	if err := json.Compact(&got, own[0].Obs.Body); err != nil || got.String() != string(weekly(40)) {
		t.Errorf("handed on %s, want this machine's own 40%% reading", own[0].Obs.Body)
	}

	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cachedAt := time.Now().Add(-time.Minute).UnixMilli() + 123
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{"oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"},
	  "cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":"uuid-1","utilization":%s}}`, cachedAt, weekly(70)))
	store := state.Open(cfg)
	key := state.Key{UUID: "uuid-1", Name: "primary"}
	at := time.Now().Add(-time.Hour)
	dec, _ := store.Claim([]state.Key{key}, at)
	store.Complete(key, dec[0].Generation, state.OutcomeStored, weekly(40), at)
	disk := Read(cfg, store, time.Now())
	own = OwnReadings(disk.Set, disk.Store, time.Now())
	if len(own) != 1 || own[0].Obs.FetchedAtMS != cachedAt || string(own[0].Obs.Body) != string(weekly(70)) {
		t.Errorf("own readings = %+v, want Claude Code's newer cache, its body and its milliseconds", own)
	}
}
