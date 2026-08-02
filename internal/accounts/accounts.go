// Package accounts discovers Claude Code accounts from the filesystem.
// There is no account list anywhere: the default ~/.claude plus every dir
// under the accounts root is the registry — the same tree any shell
// integration globs, so the two views can't drift.
package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/tag"
)

type Account struct {
	ConfigDir string // "" = the primary ~/.claude (Claude Code's default dir)
	Name      string // dir basename (the email), or PrimaryName for the primary
	Email     string // what .claude.json reports as actually logged in ("" = none)

	// Meta is the rest of that same .claude.json read, carried so callers
	// needing the cached usage payload don't re-parse a file Claude Code
	// rewrites constantly.
	Meta Meta
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
		a.Meta, _ = ReadMeta(a.MetaPath(cfg))
		a.Email = a.Meta.Email
		accts = append(accts, a)
	}
	return accts
}

// isDir follows symlinks, like the shell glob the launchers use.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Meta is everything headroom reads out of one account's .claude.json. Claude
// Code keeps two useful things there: who is logged in, and — free of charge —
// the last usage response it fetched for itself.
type Meta struct {
	Email   string
	EmailOK bool

	// CachedUsage is the raw `cachedUsageUtilization.utilization` object,
	// byte-identical in shape to the live endpoint's body, so it goes to
	// usage.ParseLimits unchanged rather than earning a second parser.
	// Nil unless CacheState is tag.OK.
	CachedUsage []byte
	FetchedAtMS int64

	// CacheState distinguishes a cache that isn't there (tag.None — ordinary;
	// Claude Code writes one only once it has fetched) from one that is there
	// but unusable (tag.Bad — its provenance couldn't be established). Both
	// leave CachedUsage nil, and without this tag `check` could not tell the
	// silent loss of the fallback from its normal absence.
	CacheState tag.State
}

// ReadMeta parses one account's .claude.json.
//
// The cached usage block is only handed back when its accountUuid matches the
// logged-in account's. A config dir that has been re-logged to a different
// account keeps the previous account's cache until Claude Code overwrites it,
// and rendering account X's quota under account Y's name is a worse failure
// than showing nothing — wrong data beats missing data only for someone who
// isn't about to pick an account based on it.
func ReadMeta(metaPath string) (Meta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return Meta{}, err
	}
	var doc struct {
		OauthAccount struct {
			EmailAddress *string `json:"emailAddress"`
			AccountUUID  string  `json:"accountUuid"`
		} `json:"oauthAccount"`
		CachedUsage *struct {
			FetchedAtMS int64           `json:"fetchedAtMs"`
			AccountUUID string          `json:"accountUuid"`
			Utilization json.RawMessage `json:"utilization"`
		} `json:"cachedUsageUtilization"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Meta{}, err
	}
	// Explicitly None: tag.OK is tag.State's zero value, so leaving this
	// implicit would have an account with no cache at all claim to have a
	// valid one.
	m := Meta{CacheState: tag.None}
	if doc.OauthAccount.EmailAddress != nil {
		m.Email, m.EmailOK = *doc.OauthAccount.EmailAddress, true
	}
	if c := doc.CachedUsage; c != nil && len(c.Utilization) > 0 {
		// Both identities must be present and agree. Accepting the cache when
		// either is missing would attribute a previous login's quota to the
		// current account — the exact failure this guard exists for — and a
		// cache with no fetch time would render as observed in 1970. If the
		// vendor stops sending either field we lose a fallback and stay
		// correct, which is the right way round — and CacheState is how
		// `check` learns the fallback went away instead of it vanishing
		// silently.
		owner := doc.OauthAccount.AccountUUID
		if owner != "" && owner == c.AccountUUID && c.FetchedAtMS > 0 {
			m.CachedUsage = c.Utilization
			m.FetchedAtMS = c.FetchedAtMS
			m.CacheState = tag.OK
		} else {
			m.CacheState = tag.Bad
		}
	}
	return m, nil
}

// MetaEmail reads .oauthAccount.emailAddress; ok reports whether the field
// is present as a string (the contract `headroom check` asserts).
func MetaEmail(metaPath string) (string, bool) {
	m, err := ReadMeta(metaPath)
	if err != nil {
		return "", false
	}
	return m.Email, m.EmailOK
}

// Local parts that never get a short launcher alias: x-<these> are reserved
// for utility commands. Shell integration that generates the launcher
// functions must reserve the same names — see DESIGN.md, "The launcher
// contract".
var reservedLocalParts = []string{"usage", "account", "account-add", "select"}

// Launcher is the command advertised for an account: x-<email> is the
// guaranteed identity; a short x-<local-part> alias exists only when the
// local part is unique among accounts and isn't the primary's name or a
// reserved utility name. Shell integration generating the actual functions
// must apply the same rule — see DESIGN.md, "The launcher contract".
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
// the only file headroom writes on the user's behalf; the other, .throttle,
// is headroom's own record of its past requests. The Keychain and every other
// piece of Claude Code's state are never touched.
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
