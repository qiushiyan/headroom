package accounts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// secondHome is a home that is not the user's login home: its primary is
// spelled out, and its own logins sit in its own dirs.
func secondHome(t *testing.T) config.Scope {
	t.Helper()
	s := config.ForHome(t.TempDir()).Claude
	s.PrimaryExplicit = true
	return s
}

// Spelled out, the primary is still the primary — it owns the store, is never
// removed and is what `.current` absent means — but everything that reads or
// launches it names its dir: the identity inside the dir, the Keychain item
// keyed on the dir, CLAUDE_CONFIG_DIR set to the dir.
func TestASecondHomesPrimaryIsSpelledOut(t *testing.T) {
	cfg := secondHome(t)
	if err := os.MkdirAll(cfg.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// The login home keeps its primary's identity at ~/.claude.json; a second
	// home's primary keeps it inside its dir, where Claude Code writes it once
	// CLAUDE_CONFIG_DIR is set. A file at the second home's ~/.claude.json is
	// not this primary's and must not be read as it.
	write := func(path, email, uuid string) {
		if err := os.WriteFile(path, []byte(`{"oauthAccount":{"emailAddress":"`+email+`","accountUuid":"`+uuid+`"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfg.PrimaryDir(), ".claude.json"), "yan@planlab.ai", "u-yan")
	write(filepath.Join(cfg.Home, ".claude.json"), "owner@x.com", "u-owner")

	set := Discover(cfg)
	p := set.Accounts[0]
	if !p.IsPrimary() || p.ConfigDir != cfg.PrimaryDir() || p.Dir() != cfg.PrimaryDir() {
		t.Fatalf("primary = %+v", p)
	}
	if p.Email != "yan@planlab.ai" || p.AccountID != "u-yan" || p.Name != "yan" {
		t.Errorf("primary identity read from the wrong file: %q %q %q", p.Email, p.AccountID, p.Name)
	}
	if err := VerifyTopology(p); err != nil {
		t.Errorf("the primary owns the store; topology: %v", err)
	}
	if a, err := set.Select(""); err != nil || a.ConfigDir != cfg.PrimaryDir() {
		t.Errorf("absent .current = %+v %v, want the spelled-out primary", a, err)
	}
	// The second home's own sessions inherit its primary's dir: that is its
	// ordinary environment, not an anomaly to announce.
	if !set.KnownDir(cfg.PrimaryDir()) {
		t.Error("a second home's own primary dir read as unexplained")
	}
}

// No share mode links a login between two dirs: not the whitelist, and not a
// config package that happens to hold a login, a history or a registry.
func TestNoLoginIsLinkedBetweenDirs(t *testing.T) {
	for name, opt := range map[string]func(cfg config.Scope) SeedOptions{
		"whitelist": func(cfg config.Scope) SeedOptions {
			return SeedOptions{ShareFrom: cfg.PrimaryDir(), ShareNames: SharedConfigEntries(config.Claude)}
		},
		"every entry": func(cfg config.Scope) SeedOptions { return SeedOptions{ShareFrom: cfg.PrimaryDir()} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := secondHome(t)
			src := cfg.PrimaryDir()
			for _, d := range []string{"skills", "sessions", "projects"} {
				if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []string{"settings.json", ".credentials.json", ".claude.json", "history.jsonl"} {
				if err := os.WriteFile(filepath.Join(src, f), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			dir, shared, kept, err := SeedKept(cfg, "a@x.com", opt(cfg))
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"settings.json", "skills"} {
				if !slices.Contains(shared, n) {
					t.Errorf("%s not shared: %v", n, shared)
				}
			}
			for _, n := range []string{".credentials.json", ".claude.json", "history.jsonl", "sessions"} {
				if slices.Contains(shared, n) {
					t.Errorf("%s was linked", n)
				}
				if fi, err := os.Lstat(filepath.Join(dir, n)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					t.Errorf("%s/%s is a link", dir, n)
				}
			}
			if name == "every entry" && !slices.Contains(kept, ".credentials.json") {
				t.Errorf("kept = %v — a login the source held must be named as kept", kept)
			}
			if err := VerifyTopology(Account{Scope: cfg, ConfigDir: dir, Name: "a@x.com"}); err != nil {
				t.Errorf("topology: %v", err)
			}
		})
	}
}

// `.ledger` is written with the rule Load reads it by, and naming this home's
// own root removes it.
func TestShareLedger(t *testing.T) {
	cfg := testConfig(t)
	codex := config.ForHome(cfg.Home).Codex
	other := t.TempDir()
	if _, err := ShareLedger(cfg, codex, "relative"); err == nil {
		t.Error("a relative root was recorded")
	}
	if _, err := os.Stat(cfg.LedgerFile()); err == nil {
		t.Fatal("a refused root left a file")
	}
	root, err := ShareLedger(cfg, codex, other+"/")
	if err != nil || root != other {
		t.Fatalf("share: %q %v", root, err)
	}
	if data, _ := os.ReadFile(cfg.LedgerFile()); string(data) != other+"\n" {
		t.Errorf(".ledger = %q", data)
	}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, err := ShareLedger(cfg, codex, cfg.AccountsRoot); err != nil || root != "" {
		t.Fatalf("own root: %q %v", root, err)
	}
	if _, err := os.Stat(cfg.LedgerFile()); err == nil {
		t.Error("naming this home's own root left .ledger behind")
	}
	if _, err := ShareLedger(codex, cfg, other); err == nil {
		t.Error("a Codex home was given a shared ledger")
	}
}
