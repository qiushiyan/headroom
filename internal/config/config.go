// Package config resolves where accounts live. The defaults suit the
// author's machine; HEADROOM_* environment variables override each of them
// for other setups.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Home         string
	AccountsRoot string // one dir per extra subscription, keyed by email
	PrimaryName  string // launcher name advertised for the primary ~/.claude
	UsageURL     string
}

func Load() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	// HEADROOM_HOME re-points everything derived from the home directory —
	// the primary config dir, the session store, the accounts root. The pty
	// harness depends on it: without it a resume test would list (and a
	// delete test would delete) the real machine's transcripts.
	if v := os.Getenv("HEADROOM_HOME"); v != "" {
		home = v
	}
	c := Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
		UsageURL:     "https://api.anthropic.com/api/oauth/usage",
	}
	if v := os.Getenv("HEADROOM_ACCOUNTS_ROOT"); v != "" {
		c.AccountsRoot = v
	}
	if v := os.Getenv("HEADROOM_PRIMARY_NAME"); v != "" {
		c.PrimaryName = v
	}
	if v := os.Getenv("HEADROOM_USAGE_URL"); v != "" {
		c.UsageURL = v
	}
	return c
}

// StateFile records which account bare `x` targets.
func (c Config) StateFile() string { return filepath.Join(c.AccountsRoot, ".current") }

// OrderFile sets dashboard display order after the primary (optional, one
// email per line; unlisted accounts follow alphabetically).
func (c Config) OrderFile() string { return filepath.Join(c.AccountsRoot, ".order") }

// PrimaryMeta is the .claude.json of the default ~/.claude account, which
// Claude Code keeps at ~/.claude.json — not inside the config dir.
func (c Config) PrimaryMeta() string { return filepath.Join(c.Home, ".claude.json") }

// PrimaryDir is the primary account's config dir. Accounts carry "" for it
// (the empty CLAUDE_CONFIG_DIR), so readers of per-account files — prompt
// history, the live-session registry — resolve the real path through here.
func (c Config) PrimaryDir() string { return filepath.Join(c.Home, ".claude") }

// ProjectsDir is the canonical machine-global session store. Every account's
// projects/ symlinks to it, so this one tree is the whole session registry.
func (c Config) ProjectsDir() string { return filepath.Join(c.Home, ".claude", "projects") }

// OwnersFile records explicit session re-homes — the third and last file
// headroom owns, beside .current and .throttle.
func (c Config) OwnersFile() string { return filepath.Join(c.AccountsRoot, ".owners") }
