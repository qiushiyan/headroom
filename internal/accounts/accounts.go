// Package accounts discovers Claude Code accounts from the filesystem.
// There is no account list anywhere: the default ~/.claude plus every dir
// under the accounts root is the registry — the same tree any shell
// integration globs, so the two views can't drift.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/tag"
)

type Account struct {
	ConfigDir string // "" = the primary ~/.claude (Claude Code's default dir)
	Name      string // dir basename (the email), or PrimaryName(cfg, meta) for the primary
	Email     string // what .claude.json reports as actually logged in ("" = none)

	// Meta is the rest of that same .claude.json read, carried so callers
	// needing the cached usage payload don't re-parse a file Claude Code
	// rewrites constantly.
	Meta Meta
}

func (a Account) IsPrimary() bool { return a.ConfigDir == "" }

// Dir is the account's real config dir. ConfigDir is "" for the primary —
// the value CLAUDE_CONFIG_DIR takes — so per-account files that live inside
// the dir (prompt history, the live-session registry) resolve through here.
func (a Account) Dir(cfg config.Config) string {
	if a.IsPrimary() {
		return cfg.PrimaryDir()
	}
	return a.ConfigDir
}

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
			if LockArtifact(line) {
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
		if strings.HasPrefix(e.Name(), ".") || LockArtifact(e.Name()) {
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
		a.Meta, _ = ReadMeta(a.MetaPath(cfg))
		a.Email = a.Meta.Email
		if d == "" {
			a.Name = PrimaryName(cfg, a.Meta)
		} else {
			a.Name = filepath.Base(d)
		}
		accts = append(accts, a)
	}
	return accts
}

// LockArtifact reports whether a name in the accounts root is vendor lock
// debris, never an account. Claude Code's config locking creates `<path>.lock`
// *directories* beside what it locks, and a crash strands them at the root —
// where discovery would otherwise adopt one as an account, whereupon the
// health probe's `claude auth status` initializes a skeleton `.claude.json`
// inside and the artifact starts rendering as a hollow login (observed
// 2026-08-10, `<email>.lock` beside the real dir). An email never ends in
// `.lock`; a dir named that way is debris, skipped here and named by `check`
// so it disappears from the board without disappearing from sight.
func LockArtifact(name string) bool { return strings.HasSuffix(name, ".lock") }

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

	// AccountUUID is the vendor's identity for the logged-in account. It is
	// what the request ledger keys on: the endpoint's budget is per account,
	// so two config dirs logged into the same account share one bucket, and
	// keying by dir name would have them double-spend it invisibly. Empty when
	// the field is absent — the ledger falls back to the dir name and simply
	// loses that sharing.
	AccountUUID string

	// Readable distinguishes "this file parsed and reports no UUID" from "this
	// file could not be read at all". Both leave AccountUUID empty, and only
	// the first may fall back to the dir name: Claude Code rewrites
	// .claude.json constantly, so a torn read would otherwise move the ledger
	// key to a second, empty bucket and spend the same account's budget twice
	// within seconds.
	Readable bool

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
	m := Meta{CacheState: tag.None, AccountUUID: doc.OauthAccount.AccountUUID, Readable: true}
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

// PrimaryName is the name the primary ~/.claude answers to. An explicit
// HEADROOM_PRIMARY_NAME wins; otherwise the local part of the email the
// primary's .claude.json says is logged in — the same identity the extras
// carry as their dir basename, minus the domain, so it can never collide
// with an extra (dir names contain "@"; local parts do not). A primary that
// has never logged in has no email to be named by and is called "primary".
//
// The derived name is only as stable as the login behind it: `.current`
// records the *name*, so a primary logout after `.current` was pinned to it
// makes Select refuse (fail-closed, as for any unmatched name) until the
// board repicks — or the name is pinned by the variable.
func PrimaryName(cfg config.Config, meta Meta) string {
	if cfg.PrimaryName != "" {
		return cfg.PrimaryName
	}
	if local, _, ok := strings.Cut(meta.Email, "@"); ok && local != "" {
		return local
	}
	return "primary"
}

// Launcher is the command advertised for an account: the guaranteed identity
// only — x-<email>, or x-<primary name>. Short local-part aliases (x-yan)
// are a shell convenience headroom neither knows nor advertises: the old
// shared naming policy ("keep the two in sync") was a cross-repo contract
// that could drift, and the board promising a command that resolves is worth
// more than promising the shortest one.
func Launcher(a Account) string { return "x-" + a.Name }

// KnownExtraDir reports whether dir is exactly some discovered non-primary
// account's config dir — the value a managed launch on that account exports,
// and therefore the value every shell *inside* such a session inherits.
// That inheritance is this machine's ordinary environment, not an anomaly:
// surfaces use this to keep their ambient-variable notices for values that
// cannot be explained that way (relative, unknown, or the primary dir
// spelled explicitly — present-but-primary is not the verified absent
// state). check still reports every present value; this only quiets the
// board and the launch line for the case that is true all day.
func KnownExtraDir(accts []Account, dir string) bool {
	for _, a := range accts {
		if !a.IsPrimary() && a.ConfigDir == dir {
			return true
		}
	}
	return false
}

// Select resolves a selector to a discovered account, strictly. selector ""
// means the recorded choice: an absent .current is the documented fresh-start
// default (the primary), but an empty, unreadable or unmatched one is an
// error — never a silent primary. This is the one resolver of `.current`,
// and it fails closed on purpose: the old shell fallback turned a torn
// `.current` or a deleted account into "launch the primary with permissions
// bypassed", which makes corruption indistinguishable from a valid choice.
// Display surfaces treat the error as "no current account" and mark nothing.
func Select(cfg config.Config, accts []Account, selector string) (Account, error) {
	name := selector
	if name == "" {
		data, err := os.ReadFile(cfg.CurrentFile())
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return primaryOf(accts)
		case err != nil:
			return Account{}, fmt.Errorf(".current could not be read (%v)", err)
		}
		name = strings.TrimRight(string(data), "\n")
		if name == "" {
			return Account{}, errors.New(".current is empty — run headroom accounts")
		}
	}
	var matches []Account
	for _, a := range accts {
		if a.Name == name {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Account{}, fmt.Errorf("%q is not a discovered account — run headroom accounts", name)
	}
	// Two accounts answer to one name — an extra dir named like the primary
	// (dir basenames are otherwise unique). Returning the first match would
	// route the name to whichever discovery listed first, silently; ambiguity
	// refuses, same policy as corrupt `.current`. Discovery stays total so
	// both rows still render — one bad dir must not become a process failure.
	return Account{}, fmt.Errorf("%q names %d accounts (an extra dir shares the primary's name) — rename %s", name, len(matches), matches[1].ConfigDir)
}

func primaryOf(accts []Account) (Account, error) {
	for _, a := range accts {
		if a.IsPrimary() {
			return a, nil
		}
	}
	// Discover always lists the primary; an account slice without one was not
	// built by discovery and cannot be resolved against.
	return Account{}, errors.New("no primary account in the discovered set")
}

// SetCurrent records the account bare `x` should target from now on,
// written atomically (temp file + rename) so a concurrent reader — a
// `headroom launch` resolving mid-write — never observes a truncated or
// empty file. The Keychain and every other piece of Claude Code's state are
// never touched.
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
	return os.Rename(tmp.Name(), cfg.CurrentFile())
}
