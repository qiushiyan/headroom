// Package accounts discovers Claude Code accounts from the filesystem.
// There is no account list anywhere: the default ~/.claude plus every dir
// under the accounts root is the registry, exactly as the zsh launchers see
// it, so the two can't drift.
package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/qiushiyan/headroom/internal/config"
)

type Account struct {
	ConfigDir string // "" = the primary ~/.claude (Claude Code's default dir)
	Name      string // dir basename (the email), or PrimaryName for the primary
	Email     string // what .claude.json reports as actually logged in ("" = none)
}

func (a Account) IsPrimary() bool { return a.ConfigDir == "" }

// MetaPath is the .claude.json recording the logged-in account.
func (a Account) MetaPath(cfg config.Config) string {
	if a.IsPrimary() {
		return cfg.PrimaryMeta()
	}
	return filepath.Join(a.ConfigDir, ".claude.json")
}

// Discover returns the primary first, then .order-listed dirs in file order,
// then the remaining dirs alphabetically.
func Discover(cfg config.Config) []Account {
	dirs := []string{""}
	seen := map[string]bool{"": true}

	if data, err := os.ReadFile(cfg.OrderFile()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line, _, _ = strings.Cut(line, "#")
			line = strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, line)
			if line == "" {
				continue
			}
			d := filepath.Join(cfg.AccountsRoot, line)
			if isDir(d) && !seen[d] {
				dirs = append(dirs, d)
				seen[d] = true
			}
		}
	}

	entries, _ := os.ReadDir(cfg.AccountsRoot)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		d := filepath.Join(cfg.AccountsRoot, e.Name())
		if isDir(d) && !seen[d] {
			dirs = append(dirs, d)
			seen[d] = true
		}
	}

	accts := make([]Account, 0, len(dirs))
	for _, d := range dirs {
		a := Account{ConfigDir: d}
		if d == "" {
			a.Name = cfg.PrimaryName
		} else {
			a.Name = filepath.Base(d)
		}
		a.Email = loggedInEmail(a.MetaPath(cfg))
		accts = append(accts, a)
	}
	return accts
}

// isDir follows symlinks, like the shell glob the launchers use.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func loggedInEmail(metaPath string) string {
	email, _ := MetaEmail(metaPath)
	return email
}

// MetaEmail reads .oauthAccount.emailAddress; ok reports whether the field
// is present as a string (the contract `headroom check` asserts).
func MetaEmail(metaPath string) (string, bool) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", false
	}
	var meta struct {
		OauthAccount struct {
			EmailAddress *string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.OauthAccount.EmailAddress == nil {
		return "", false
	}
	return *meta.OauthAccount.EmailAddress, true
}

// Local parts that never get a short launcher alias: x-<these> are utility
// commands. Mirrors CLAUDE_X_RESERVED in dotfiles claude.zsh — keep in sync.
var reservedLocalParts = []string{"usage", "account", "account-add", "select"}

// Launcher is the command to advertise for an account. Mirrors
// _claude_gen_launchers in dotfiles claude.zsh: x-<email> always exists; the
// short x-<local-part> alias exists only when the local part is unique among
// accounts and isn't the primary's name or a reserved utility name — keep
// the rule and the reserved list in sync.
func Launcher(a Account, all []Account, primaryName string) string {
	if a.IsPrimary() {
		return "x-" + primaryName
	}
	email := a.Name
	local, _, hasAt := strings.Cut(email, "@")
	if slices.Contains(reservedLocalParts, local) {
		return "x-" + email
	}
	n := 0
	for _, other := range all {
		if other.IsPrimary() {
			continue
		}
		otherLocal, _, _ := strings.Cut(other.Name, "@")
		if otherLocal == local {
			n++
		}
	}
	if hasAt && local != primaryName && n == 1 {
		return "x-" + local
	}
	return "x-" + email
}

// CurrentTarget is the account name bare `x` targets right now.
func CurrentTarget(cfg config.Config) string {
	data, err := os.ReadFile(cfg.StateFile())
	if err != nil {
		return cfg.PrimaryName
	}
	name := strings.TrimRight(string(data), "\n")
	if name == "" {
		return cfg.PrimaryName
	}
	return name
}

// SetCurrent records the account bare `x` should target from now on,
// written atomically (temp file + rename) so a concurrent reader — an `x`
// launcher mid-startup — never observes a truncated or empty file. This is
// the one file headroom writes; the Keychain is never touched.
func SetCurrent(cfg config.Config, name string) error {
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cfg.AccountsRoot, ".current-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), cfg.StateFile())
}
