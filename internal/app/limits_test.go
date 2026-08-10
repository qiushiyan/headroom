package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

// The read surface answers from disk alone. Its signature is the structural
// half of that promise — prepareRead takes no credential reader, no auth
// oracle and no HTTP client, so there is nothing it could probe — and this
// test covers the behavioral half: what is already known comes back whole,
// under honest provenance, with the two skipped questions answered as
// "unprobed" and "no attempt" rather than as silence.
func TestPrepareRead(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
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
	st := state.Open(cfg.AccountsRoot)
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
	if err := accounts.SetCurrent(cfg, "b@x.com"); err != nil {
		t.Fatal(err)
	}

	list, current := prepareRead(cfg, accounts.Discover(cfg), st.Load(), now)
	if current != "b@x.com" {
		t.Errorf("current = %q", current)
	}
	byName := byName(list)
	if len(byName) != 2 {
		t.Fatalf("got %d accounts", len(byName))
	}

	p := byName["primary"].View
	if p.Health != render.HealthUnprobed {
		t.Errorf("skipped probe must read as unprobed, got %v", p.Health)
	}
	if p.Attempt.State != render.AttemptNone {
		t.Errorf("no attempt was made, got %v", p.Attempt.State)
	}
	if p.Obs == nil || p.Obs.Source != render.SourceStore {
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
	if b.Obs == nil || b.Obs.Source != render.SourceCache || b.Obs.Rows[0].Percent != 31 {
		t.Errorf("claude cache not replayed: %+v", b.Obs)
	}
	if !b.Current {
		t.Error("current marker lost")
	}
}

// The shipped command, end to end minus the process boundary: flag policy,
// account scoping, and the emitted document — not the helpers it happens to be
// built from.
func TestLimitsCommand(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	extraDir := filepath.Join(cfg.AccountsRoot, "b@x.com")
	if err := os.MkdirAll(extraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)
	writeJSON(t, filepath.Join(extraDir, ".claude.json"), `{"oauthAccount":{"emailAddress":"b@x.com"}}`)

	var buf bytes.Buffer
	if code := runLimitsTo(&buf, cfg, []string{"--account", "b@x.com"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	accts := doc["accounts"].([]any)
	if len(accts) != 1 || accts[0].(map[string]any)["name"] != "b@x.com" {
		t.Fatalf("scoped document = %v", doc["accounts"])
	}
	if accts[0].(map[string]any)["health"] != "unprobed" {
		t.Errorf("health = %v", accts[0].(map[string]any)["health"])
	}

	// The refusals, same policy as launch and resolve: an unknown name is an
	// error, never an empty success, and "" is not a name.
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"--account", "nobody@x.com"}, 1},
		{[]string{"--account", ""}, 2},
		{[]string{"--account"}, 2},
		{[]string{"--frobnicate"}, 2},
	} {
		buf.Reset()
		if code := runLimitsTo(&buf, cfg, c.args); code != c.code {
			t.Errorf("args %v: exit %d, want %d", c.args, code, c.code)
		}
		if buf.Len() != 0 {
			t.Errorf("args %v: refused command must emit no document, got %s", c.args, buf.Bytes())
		}
	}
}

// An unreadable store must be distinguishable from "nothing has ever been
// observed": both serialize usage as null, and only the problems section says
// which one the consumer is looking at.
func TestLimitsSurfacesStateProblems(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)
	writeJSON(t, filepath.Join(cfg.AccountsRoot, "state.json"), `not json at all`)

	var buf bytes.Buffer
	if code := runLimitsTo(&buf, cfg, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Problems []struct{ Section, Detail string } `json:"problems"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Problems) == 0 {
		t.Fatal("corrupt store invisible: a first run and an unreadable file serialize alike")
	}

	// And a healthy empty store carries none — the section is a signal, not
	// a fixture.
	if err := os.Remove(filepath.Join(cfg.AccountsRoot, "state.json")); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if code := runLimitsTo(&buf, cfg, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var clean map[string]any
	if err := json.Unmarshal(buf.Bytes(), &clean); err != nil {
		t.Fatal(err)
	}
	if _, present := clean["problems"]; present {
		t.Errorf("empty store must omit problems: %v", clean["problems"])
	}
}
