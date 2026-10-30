package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// pinned is cfg with Claude Code's primary pinned to name, as a shell that
// exported that HEADROOM_PRIMARY_NAME runs headroom.
func pinned(cfg config.Config, name string) config.Config {
	cfg.Claude.PrimaryName = name
	return cfg
}

// A shell started before the rename still pins the old name. It moves
// nothing back, runs under the recorded name, and its old launcher's
// `--account qiushi` still reaches the primary.
func TestAStalePinNeverMovesTheRecordsBack(t *testing.T) {
	cfg := renameHome(t)
	SettlePrimaryNames(cfg, &bytes.Buffer{})
	statePath := filepath.Join(cfg.Claude.AccountsRoot, "state.json")
	before := readFile(t, statePath)

	var out bytes.Buffer
	eff := SettlePrimaryNames(pinned(cfg, "qiushi"), &out)
	if out.Len() != 0 {
		t.Errorf("a stale run said %q", out.String())
	}
	if after := readFile(t, statePath); after != before {
		t.Errorf("a stale run rewrote state.json")
	}
	if got := readFile(t, cfg.Claude.CurrentFile()); got != "qiushi@planlab.ai\n" {
		t.Errorf(".current: %q", got)
	}
	if eff.Claude.PrimaryName != "qiushi@planlab.ai" || !slices.Contains(eff.Claude.PrimaryAliases, "qiushi") {
		t.Errorf("effective scope: %q aliases %v", eff.Claude.PrimaryName, eff.Claude.PrimaryAliases)
	}
	set := accounts.Discover(eff.Claude)
	for _, sel := range []string{"", "qiushi", "qiushi@planlab.ai"} {
		if a, err := set.Select(sel); err != nil || !a.IsPrimary() || a.Name != "qiushi@planlab.ai" {
			t.Errorf("select %q: %+v (%v)", sel, a, err)
		}
	}
	if o, _ := state.Open(cfg.Claude).Load().Owner("s-primary"); o.Account != "qiushi@planlab.ai" {
		t.Errorf("the re-home: %+v", o)
	}
	if got := state.Open(cfg.Claude).RecordedFormer(); !slices.Equal(got, []string{"qiushi"}) {
		t.Errorf("former: %v", got)
	}
}

// New and old shells alternating move the records once, forward.
func TestAlternatingShellsMoveThePrimaryOnce(t *testing.T) {
	cfg := renameHome(t)
	var out bytes.Buffer
	for _, name := range []string{"qiushi@planlab.ai", "qiushi", "qiushi@planlab.ai", "qiushi"} {
		SettlePrimaryNames(pinned(cfg, name), &out)
	}
	if n := strings.Count(out.String(), "moved its records"); n != 1 {
		t.Errorf("%d moves: %q", n, out.String())
	}
	st := state.Open(cfg.Claude)
	if got := st.RecordedPrimary(); got != "qiushi@planlab.ai" {
		t.Errorf("recorded: %q", got)
	}
	if o, _ := st.Load().Owner("s-primary"); o.Account != "qiushi@planlab.ai" {
		t.Errorf("the re-home: %+v", o)
	}
}

// A home the build before former names wrote records the new name alone; the
// name its records had before any pin, the login's local part, counts as a
// former one, so an old shell there moves nothing either.
func TestAHomeWithoutFormerNamesKeepsTheNewName(t *testing.T) {
	cfg := renameHome(t)
	SettlePrimaryNames(cfg, &bytes.Buffer{})
	statePath := filepath.Join(cfg.Claude.AccountsRoot, "state.json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, statePath)), &doc); err != nil {
		t.Fatal(err)
	}
	doc["primary"] = map[string]any{"name": "qiushi@planlab.ai"}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(statePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	eff := SettlePrimaryNames(pinned(cfg, "qiushi"), &out)
	if out.Len() != 0 || state.Open(cfg.Claude).RecordedPrimary() != "qiushi@planlab.ai" {
		t.Errorf("moved back: %q, recorded %q", out.String(), state.Open(cfg.Claude).RecordedPrimary())
	}
	if eff.Claude.PrimaryName != "qiushi@planlab.ai" {
		t.Errorf("effective name: %q", eff.Claude.PrimaryName)
	}
}

// A home an old shell already moved back (records and recorded name under
// the local part, nothing recorded as former) moves forward at the next new
// shell, which records the old name as former; old shells stop there.
func TestARevertedHomeMovesForwardOnce(t *testing.T) {
	cfg := renameHome(t) // records under "qiushi", nothing recorded
	SettlePrimaryNames(pinned(cfg, "qiushi"), &bytes.Buffer{})
	if got := state.Open(cfg.Claude).RecordedPrimary(); got != "qiushi" {
		t.Fatalf("setup: recorded %q", got)
	}
	var out bytes.Buffer
	SettlePrimaryNames(pinned(cfg, "qiushi@planlab.ai"), &out)
	SettlePrimaryNames(pinned(cfg, "qiushi"), &out)
	if n := strings.Count(out.String(), "moved its records from qiushi"); n != 1 {
		t.Errorf("%d moves: %q", n, out.String())
	}
	st := state.Open(cfg.Claude)
	if st.RecordedPrimary() != "qiushi@planlab.ai" || !slices.Equal(st.RecordedFormer(), []string{"qiushi"}) {
		t.Errorf("recorded %q former %v", st.RecordedPrimary(), st.RecordedFormer())
	}
}

// Codex keeps its primary's newest name the same way.
func TestAStaleCodexPinMovesNothing(t *testing.T) {
	home := t.TempDir()
	cfg := config.ForHome(home)
	scope := cfg.Codex
	codexauthtest.Login{Email: "qiushi@planlab.ai", AccountID: "acc-q", UserID: "user-q"}.Write(t, scope.PrimaryDir())
	if err := os.MkdirAll(scope.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, scope.CurrentFile(), "qiushi\n")
	var out bytes.Buffer
	for _, name := range []string{"qiushi@planlab.ai", "qiushi", "qiushi@planlab.ai", "qiushi"} {
		c := cfg
		c.Codex.PrimaryName = name
		eff := SettlePrimaryNames(c, &out)
		if eff.Codex.PrimaryName != "qiushi@planlab.ai" {
			t.Errorf("pin %q: effective %q", name, eff.Codex.PrimaryName)
		}
		if a, err := accounts.Discover(eff.Codex).Select(""); err != nil || !a.IsPrimary() {
			t.Errorf("pin %q: .current resolves to %+v (%v)", name, a, err)
		}
	}
	if n := strings.Count(out.String(), "moved its records"); n != 1 {
		t.Errorf("%d moves: %q", n, out.String())
	}
	if got := readFile(t, scope.CurrentFile()); got != "qiushi@planlab.ai\n" {
		t.Errorf(".current: %q", got)
	}
}
