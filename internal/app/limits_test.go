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
	cfg := claudeScope(home, "primary")
	extraDir := filepath.Join(cfg.AccountsRoot, "b@x.com")
	if err := os.MkdirAll(extraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)
	writeJSON(t, filepath.Join(extraDir, ".claude.json"), `{"oauthAccount":{"emailAddress":"b@x.com"}}`)

	var buf bytes.Buffer
	if got := call(func(std Streams) error { return Limits(std, []config.Scope{cfg}, "b@x.com") }); got.code != 0 {
		t.Fatalf("exit %d", got.code)
	} else {
		buf.WriteString(got.stdout)
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

	// An unknown name is an error, never an empty success. ("" is not a name
	// either: the command line refuses it, cmd/headroom's TestGrammar.)
	if got := call(func(std Streams) error { return Limits(std, []config.Scope{cfg}, "nobody@x.com") }); got.code != 1 || got.stdout != "" {
		t.Errorf("an unknown name: exit %d, document %q", got.code, got.stdout)
	}
}
