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

// The primary's name is not compiled in: unset, Load leaves it "" so
// discovery derives it from the primary's login; the variable pins it.
func TestLoadPrimaryNameIsDerivedUnlessPinned(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "")
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_PRIMARY_NAME", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PrimaryName != "" {
		t.Errorf("unset HEADROOM_PRIMARY_NAME: PrimaryName = %q, want \"\" (derive at discovery)", c.PrimaryName)
	}
	t.Setenv("HEADROOM_PRIMARY_NAME", "pinned")
	c, _ = Load()
	if c.PrimaryName != "pinned" {
		t.Errorf("pinned: PrimaryName = %q", c.PrimaryName)
	}
}

// The advertised launcher spelling defaults to a command every install has
// and is re-spelled by HEADROOM_LAUNCHER_FORMAT, which must carry exactly
// one %s — a malformed format would render every hint wrong.
func TestLoadLauncherFormat(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "")
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_LAUNCHER_FORMAT", "")
	c, err := Load()
	if err != nil || c.LauncherFormat != "headroom launch --account %s" {
		t.Errorf("default LauncherFormat = %q, %v", c.LauncherFormat, err)
	}
	t.Setenv("HEADROOM_LAUNCHER_FORMAT", "x-%s")
	if c, err = Load(); err != nil || c.LauncherFormat != "x-%s" {
		t.Errorf("pinned LauncherFormat = %q, %v", c.LauncherFormat, err)
	}
	for _, bad := range []string{"x-", "%s %s", "%d-%s", "100%% %s"} {
		t.Setenv("HEADROOM_LAUNCHER_FORMAT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("HEADROOM_LAUNCHER_FORMAT=%q accepted", bad)
		}
	}
}
