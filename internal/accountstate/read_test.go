package accountstate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestReadDiskFacts(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com","accountUuid":"uuid-1"}}`)
	extraDir := filepath.Join(cfg.AccountsRoot, "b@x.com")
	if err := os.MkdirAll(extraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchedAt := time.Now().Add(-20 * time.Hour)
	writeJSON(t, filepath.Join(extraDir, ".claude.json"), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"b@x.com","accountUuid":"uuid-2"},
	  "cachedUsageUtilization":{
	    "fetchedAtMs":%d,"accountUuid":"uuid-2",
	    "utilization":{"limits":[{"kind":"session","group":"session","percent":31}]}}}`,
		fetchedAt.UnixMilli()))

	// Seed the store the way the store gets seeded: a claimed, completed
	// fetch. The claim's spacing is then live, which is exactly the state a
	// statusline refresher reads in — figures present, next fetch not yet due.
	st := state.Open(cfg)
	now := time.Now()
	key := state.Key{UUID: "uuid-1", Name: "primary"}
	decs, err := st.Claim([]state.Key{key}, now)
	if err != nil || !decs[0].Permit {
		t.Fatalf("seed claim: %v %+v", err, decs)
	}
	body := `{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":81,
	  "scope":{"model":{"id":null,"display_name":"Fable"}},"severity":"normal"}]}`
	if _, err := st.Complete(key, decs[0].Generation, state.OutcomeStored, []byte(body), now); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Discover(cfg).SetCurrent(accounts.Account{Scope: cfg, Name: "b@x.com"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", t.TempDir())
	disk := Read(cfg, st, now)
	list, current := disk.Accounts, disk.Current
	if current != "b@x.com" {
		t.Errorf("current = %q", current)
	}
	byName := map[string]Account{}
	for _, a := range list {
		byName[a.Acct.Name] = a
	}
	if len(byName) != 2 {
		t.Fatalf("got %d accounts", len(byName))
	}

	p := byName["primary"].View
	if p.Health != HealthUnprobed {
		t.Errorf("skipped probe must read as unprobed, got %v", p.Health)
	}
	if p.Attempt.State != AttemptNone {
		t.Errorf("no attempt was made, got %v", p.Attempt.State)
	}
	if p.Obs == nil || p.Obs.Source != SourceStore {
		t.Fatalf("stored observation not replayed: %+v", p.Obs)
	}
	r := p.Obs.Rows[0]
	if r.Kind != "weekly_scoped" || r.Model != "Fable" || r.Percent != 81 {
		t.Errorf("row = %+v", r)
	}
	if p.Attempt.NextEligibleAt <= now.Unix() {
		t.Error("live spacing not surfaced as the advisory next-eligible instant")
	}

	b := byName["b@x.com"].View
	if b.Obs == nil || b.Obs.Source != SourceCache || b.Obs.Rows[0].Percent != 31 {
		t.Errorf("claude cache not replayed: %+v", b.Obs)
	}
	if !b.Current {
		t.Error("current marker lost")
	}
}

func TestReadKeepsZeroRowCache(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"p@x.com","accountUuid":"u"},
	  "cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":"u",
	    "utilization":{"limits":[]}}}`, time.Now().UnixMilli()))
	v := Read(cfg, state.Open(cfg), time.Now()).Accounts[0].View
	if v.Obs == nil {
		t.Fatal("a zero-row cache was discarded instead of shown as 'no limits'")
	}
	if len(v.Obs.Rows) != 0 || v.Obs.Source != SourceCache {
		t.Errorf("observation: %+v", v.Obs)
	}
}

func TestReadSelectsTheNewestObservation(t *testing.T) {
	body := func(pct int) []byte {
		return fmt.Appendf(nil, `{"limits":[{"kind":"session","percent":%d}]}`, pct)
	}
	const ours, theirs = 10, 20

	cases := []struct {
		name       string
		ownAge     time.Duration // 0 = no stored observation
		cacheAge   time.Duration // 0 = no Claude Code cache
		wantPct    int           // 0 = nothing shown
		wantSource Source
	}{
		{"our seconds-old fetch beats a day-old cache", 5 * time.Second, 37 * time.Hour, ours, SourceStore},
		{"a fresh cache beats our stale fetch", 48 * time.Hour, time.Hour, theirs, SourceCache},
		{"ours alone", 30 * time.Second, 0, ours, SourceStore},
		{"theirs alone", 0, 2 * time.Hour, theirs, SourceCache},
		{"neither", 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := claudeScope(home, "primary")
			if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			meta := `{"oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"}}`
			if c.cacheAge != 0 {
				meta = fmt.Sprintf(`{"oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"},
				  "cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":"uuid-1",
				    "utilization":%s}}`, now.Add(-c.cacheAge).UnixMilli(), body(theirs))
			}
			writeJSON(t, cfg.PrimaryMeta(), meta)

			store := state.Open(cfg)
			key := state.Key{UUID: "uuid-1", Name: "primary"}
			var storedAt time.Time
			if c.ownAge != 0 {
				storedAt = now.Add(-c.ownAge)
				dec, err := store.Claim([]state.Key{key}, storedAt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Complete(key, dec[0].Generation, state.OutcomeStored, body(ours), storedAt); err != nil {
					t.Fatal(err)
				}
			}
			v := Read(cfg, store, now).Accounts[0].View

			if c.wantPct == 0 {
				if v.Obs != nil {
					t.Fatalf("invented an observation: %+v", v.Obs)
				}
				return
			}
			if v.Obs == nil {
				t.Fatal("no observation chosen")
			}
			if v.Obs.Rows[0].Percent != c.wantPct || v.Obs.Source != c.wantSource {
				t.Errorf("chose %d%% from source %v; want %d%% from %v",
					v.Obs.Rows[0].Percent, v.Obs.Source, c.wantPct, c.wantSource)
			}
			if c.wantSource == SourceStore && v.Obs.ObservedAt != storedAt.Unix() {
				t.Errorf("a replayed observation was restamped: got %d want %d",
					v.Obs.ObservedAt, storedAt.Unix())
			}
		})
	}
}

func TestActionableRequiresHealthAndFreshness(t *testing.T) {
	now := time.Now().Unix()
	fresh := &Observation{Rows: []usage.Row{{Label: "5h session"}}, ObservedAt: now - 5}
	if (Facts{Health: HealthNoLogin, Obs: fresh}).Actionable(now) {
		t.Error("a logged-out account with a fresh cache was reported as actionable")
	}
	if !(Facts{Health: HealthOK, Obs: fresh}).Actionable(now) {
		t.Error("a healthy account with fresh figures should be actionable")
	}
	old := &Observation{Rows: []usage.Row{{Label: "5h session"}}, ObservedAt: now - 100000}
	if (Facts{Health: HealthOK, Obs: old}).Actionable(now) {
		t.Error("stale figures are not grounds for a choice")
	}
}
