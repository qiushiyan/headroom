package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

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
