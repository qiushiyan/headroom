// Package config resolves where accounts live. The defaults suit the
// author's machine; HEADROOM_* environment variables override each of them
// for other setups.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Home         string
	AccountsRoot string // one dir per extra subscription, keyed by email
	// PrimaryName is the name the primary ~/.claude answers to — what
	// `.current` records, `--account` selects and the board's launcher column
	// shows. Set explicitly by HEADROOM_PRIMARY_NAME; "" means *derive it at
	// discovery* from the primary's logged-in email (accounts.PrimaryName), so
	// a fresh install needs no configuration and the primary is named the way
	// the extras are — by who is logged in. Pin it when the derived name must
	// survive a primary logout (`.current` stores the name, not the dir).
	PrimaryName string
	UsageURL     string

	// PrimaryRelocated records that HEADROOM_HOME points somewhere other than
	// the process's real home. Observation surfaces keep working against the
	// configured tree — that is what the pty harness exists on — but a
	// *primary launch* must refuse: the primary is selected by
	// CLAUDE_CONFIG_DIR being absent, which the vendor resolves against the
	// real home, so the child would run on a tree the board never described.
	PrimaryRelocated bool
}

// Load resolves the configuration, refusing relative path overrides outright.
//
// The refusal is load-bearing, not pedantry: verified against the 2.1.220
// binary, a relative CLAUDE_CONFIG_DIR makes claude write its config state
// into a cwd-relative dir while reading credentials from the *default*
// Keychain item — the session runs as the primary under a stray state dir.
// Every config-dir path headroom builds derives from these two roots, so
// absoluteness is established here, once, at the only door.
//
// Deliberately no filepath.Clean or EvalSymlinks on an accepted value: the
// vendor keys its Keychain service name on the config dir's spelling, so
// rewriting the string could re-key every extra account's credentials.
// (filepath.Join already normalizes the per-account paths derived below,
// exactly as it always has.)
func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	realHome := home
	if v := os.Getenv("HEADROOM_HOME"); v != "" {
		if !filepath.IsAbs(v) {
			return Config{}, fmt.Errorf("HEADROOM_HOME=%q is not absolute — a relative config-dir path would make claude run as the primary while writing state beside the cwd; spell it absolute", v)
		}
		home = v
	}
	c := Config{
		Home:             home,
		AccountsRoot:     filepath.Join(home, ".claude-accounts"),
		UsageURL:         "https://api.anthropic.com/api/oauth/usage",
		PrimaryRelocated: filepath.Clean(home) != filepath.Clean(realHome),
	}
	if v := os.Getenv("HEADROOM_ACCOUNTS_ROOT"); v != "" {
		if !filepath.IsAbs(v) {
			return Config{}, fmt.Errorf("HEADROOM_ACCOUNTS_ROOT=%q is not absolute — a relative config-dir path would make claude run as the primary while writing state beside the cwd; spell it absolute", v)
		}
		c.AccountsRoot = v
	}
	if v := os.Getenv("HEADROOM_PRIMARY_NAME"); v != "" {
		c.PrimaryName = v
	}
	if v := os.Getenv("HEADROOM_USAGE_URL"); v != "" {
		c.UsageURL = v
	}
	return c, nil
}

// CurrentFile records which account bare `x` targets. headroom is its only
// reader (accounts.Select, strictly — corruption refuses, never defaults)
// and only writer (the board's enter, `launch --remember`).
//
// It is deliberately not part of state.json: one human-legible routing fact
// with a fail-closed policy of its own, kept apart from a ledger that is
// disposable and self-heals by quarantine — neither failure policy should
// govern the other.
func (c Config) CurrentFile() string { return filepath.Join(c.AccountsRoot, ".current") }

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
