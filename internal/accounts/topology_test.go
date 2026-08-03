package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

func topoConfig(t *testing.T) (config.Config, Account) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
	dir := filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, Account{ConfigDir: dir, Name: "yan@planlab.ai"}
}

// Each failure mode carries its own remedy, so the classification — not just
// the verdict — is the contract: a link resolving elsewhere is "fix by hand",
// a real directory holds unmigrated sessions, an absent link was never
// seeded, and an absent canonical store is a different message again.
func TestVerifyTopologyClassifies(t *testing.T) {
	link := func(cfg config.Config, a Account) string { return filepath.Join(a.ConfigDir, "projects") }

	t.Run("valid symlink passes", func(t *testing.T) {
		cfg, a := topoConfig(t)
		os.MkdirAll(cfg.ProjectsDir(), 0o755)
		os.Symlink(cfg.ProjectsDir(), link(cfg, a))
		if err := VerifyTopology(cfg, a); err != nil {
			t.Errorf("valid topology refused: %v", err)
		}
	})
	t.Run("primary passes vacuously", func(t *testing.T) {
		cfg, _ := topoConfig(t)
		if err := VerifyTopology(cfg, Account{Name: "qiushi"}); err != nil {
			t.Errorf("primary refused: %v", err)
		}
	})
	t.Run("missing link never seeded", func(t *testing.T) {
		cfg, a := topoConfig(t)
		os.MkdirAll(cfg.ProjectsDir(), 0o755)
		err := VerifyTopology(cfg, a)
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Errorf("err = %v, want the never-seeded message", err)
		}
	})
	t.Run("real directory is the history fork", func(t *testing.T) {
		cfg, a := topoConfig(t)
		os.MkdirAll(cfg.ProjectsDir(), 0o755)
		os.MkdirAll(link(cfg, a), 0o755)
		err := VerifyTopology(cfg, a)
		if err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Errorf("err = %v, want the unmigrated-sessions message", err)
		}
	})
	t.Run("link resolving elsewhere", func(t *testing.T) {
		cfg, a := topoConfig(t)
		os.MkdirAll(cfg.ProjectsDir(), 0o755)
		other := t.TempDir()
		os.Symlink(other, link(cfg, a))
		err := VerifyTopology(cfg, a)
		if err == nil || !strings.Contains(err.Error(), "does not resolve") {
			t.Errorf("err = %v, want the fix-by-hand message", err)
		}
	})
	t.Run("absent canonical store", func(t *testing.T) {
		cfg, a := topoConfig(t)
		err := VerifyTopology(cfg, a)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("err = %v, want the absent-store message", err)
		}
	})
	t.Run("canonical store must not be a symlink", func(t *testing.T) {
		cfg, a := topoConfig(t)
		real := t.TempDir()
		os.MkdirAll(filepath.Dir(cfg.ProjectsDir()), 0o755)
		os.Symlink(real, cfg.ProjectsDir())
		os.Symlink(cfg.ProjectsDir(), link(cfg, a))
		err := VerifyTopology(cfg, a)
		if err == nil || !strings.Contains(err.Error(), "itself a symlink") {
			t.Errorf("err = %v, want the canonical-must-be-real message", err)
		}
	})
}

// An extra dir named like the primary makes one name answer for two accounts.
// Discovery stays total (accounts fail independently; both rows render) but
// resolving that name refuses — returning the first match routed `.current`
// silently to whichever discovery listed first.
func TestSelectRefusesAmbiguousName(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, "qiushi"), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := Discover(cfg)
	if len(accts) != 2 {
		t.Fatalf("discovery dropped a row: %d accounts, want 2 (it must stay total)", len(accts))
	}
	if _, err := Select(cfg, accts, "qiushi"); err == nil {
		t.Error("an ambiguous name resolved instead of refusing")
	}
	// The unambiguous path is untouched.
	os.WriteFile(cfg.CurrentFile(), []byte("qiushi\n"), 0o644)
	if _, err := Select(cfg, accts, ""); err == nil {
		t.Error(".current naming an ambiguous account resolved instead of refusing")
	}
}
