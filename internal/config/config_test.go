package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Relative overrides refuse outright: verified against the 2.1.220 binary, a
// relative CLAUDE_CONFIG_DIR runs the session as the primary (default
// Keychain item) while writing state beside the cwd, and every config-dir
// path headroom builds derives from these roots.
func TestLoadRefusesRelativeOverrides(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "fixture")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_HOME") {
		t.Errorf("relative HEADROOM_HOME: err = %v, want a refusal naming it", err)
	}
	t.Setenv("HEADROOM_HOME", "")

	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "./accounts")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_ACCOUNTS_ROOT") {
		t.Errorf("relative HEADROOM_ACCOUNTS_ROOT: err = %v, want a refusal naming it", err)
	}
}

func TestLoadMarksRelocatedPrimary(t *testing.T) {
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_HOME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrimaryRelocated {
		t.Error("unset HEADROOM_HOME must not read as relocated")
	}

	fixture := t.TempDir()
	t.Setenv("HEADROOM_HOME", fixture)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PrimaryRelocated {
		t.Error("a re-pointed home must be marked: primary launches refuse on it")
	}
	if cfg.AccountsRoot != filepath.Join(fixture, ".claude-accounts") {
		t.Errorf("AccountsRoot = %q, want under the fixture home", cfg.AccountsRoot)
	}
}
