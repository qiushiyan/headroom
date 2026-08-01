// Package config resolves where accounts live. Defaults mirror the reference
// Bash implementation in ~/dotfiles (claude-usage + claude.zsh); HEADROOM_*
// environment variables override them for other machines.
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
