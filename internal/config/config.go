// Package config resolves where accounts live, once per vendor. The defaults
// suit the author's machine; HEADROOM_* environment variables override each
// of them for other setups.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Vendor is one of two fixed values. The set is closed on purpose: what
// varies between the two is either data on a Scope or one of three document
// readers (identity, access, the usage body) — never a method per pipeline
// stage, which would invite a third "generic" vendor nobody has.
type Vendor string

const (
	Claude Vendor = "claude"
	Codex  Vendor = "codex"
)

// Vendors is the closed set in display order: Claude Code first.
var Vendors = []Vendor{Claude, Codex}

// ParseVendor is the one reader of a `--vendor` value.
func ParseVendor(s string) (Vendor, error) {
	switch Vendor(s) {
	case Claude, Codex:
		return Vendor(s), nil
	}
	return "", fmt.Errorf("unknown vendor %q — claude or codex", s)
}

// Title is the vendor's product name, for headings and the tab bar.
func (v Vendor) Title() string {
	if v == Codex {
		return "Codex"
	}
	return "Claude Code"
}

// DefaultSpacing is the quiet period headroom keeps between requests for one
// account. Claude Code's usage endpoint budget is per account and refills in
// roughly 30-70 seconds by measurement; this sits deliberately above that,
// because headroom is a bystander on an undocumented endpoint and should err
// toward silence. Codex's budget is unmeasured and starts at the same value
// as a local policy, not a vendor promise — it is a field on the scope so a
// stricter budget changes a number, and every ledger deadline respects it.
const DefaultSpacing = 90 * time.Second

// EnvPolicy is what a launched child's environment is built from, as data:
// the variable that selects a non-primary account, every inherited variable
// that could re-route the launch or substitute its credentials, and which of
// those carry secrets whose values are never printed.
type EnvPolicy struct {
	HomeVar string
	Strip   []string
	Secret  []string
}

// IsSecret reports whether name's value must never reach a notice.
func (p EnvPolicy) IsSecret(name string) bool {
	for _, s := range p.Secret {
		if s == name {
			return true
		}
	}
	return false
}

// EnvPolicy is the vendor's row of the environment policy table. The zero
// Vendor answers as Claude Code, so a forgotten construction fails toward
// the rules the existing launch surface was verified under.
func (v Vendor) EnvPolicy() EnvPolicy {
	if v == Codex {
		// CODEX_API_KEY and CODEX_ACCESS_TOKEN are read from the environment
		// ahead of auth.json (the latter measured on codex-cli 0.155.0), so an
		// inherited one would run the chosen home on someone else's login.
		return EnvPolicy{
			HomeVar: "CODEX_HOME",
			Strip:   []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
			Secret:  []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
		}
	}
	return EnvPolicy{
		HomeVar: "CLAUDE_CONFIG_DIR",
		Strip:   []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"},
	}
}

// Scope is the resolved configuration for one vendor. Everything that is
// only a path, a name or a number lives here, so the mechanisms that own an
// invariant (discovery, `.current`, topology, the claim ledger, launch
// preparation) are written once and take the scope.
type Scope struct {
	Vendor       Vendor
	Home         string
	AccountsRoot string // one dir per extra subscription, keyed by email

	// PrimaryName is the name the vendor's primary dir answers to — what
	// `.current` records, `--account` selects and the board's launcher column
	// shows. Set explicitly by HEADROOM_PRIMARY_NAME (HEADROOM_CODEX_PRIMARY_NAME
	// for Codex); "" means *derive it at discovery* from the primary's
	// logged-in email (accounts.PrimaryName), so a fresh install needs no
	// configuration and the primary is named the way the extras are — by who
	// is logged in. Pin it when the derived name must survive a primary logout
	// (`.current` stores the name, not the dir).
	PrimaryName string

	// LauncherFormat spells the command the board advertises for an account
	// (`%s` = the account's name): every "run <this> and /login" hint and
	// the header's launcher column. The default is a command that exists on
	// every install; a shell that wraps `headroom launch` in shorter names
	// sets HEADROOM_LAUNCHER_FORMAT so the board promises the spelling that
	// actually resolves there ("x-%s"). Display only — nothing routes by it.
	LauncherFormat string
	UsageURL       string

	// Spacing is the quiet period between requests for one account. Every
	// deadline the ledger computes takes it as a floor, and the board's idle
	// cadence and the fresh window follow it.
	Spacing time.Duration

	// Present reports that the vendor exists on this machine. Both scopes
	// are always resolved — `accounts add` must work before any directory
	// exists — and presence only decides whether the board, `--json` and
	// `check` include a vendor unasked. Claude Code is always present, as it
	// always was: a machine without ~/.claude renders the primary as not
	// logged in.
	Present bool

	// PrimaryRelocated records that HEADROOM_HOME points somewhere other than
	// the process's real home. Observation surfaces keep working against the
	// configured tree — that is what the pty harness exists on — but a
	// *primary launch* must refuse: the primary is selected by the vendor's
	// home variable being absent, which the vendor resolves against the real
	// home, so the child would run on a tree the board never described.
	PrimaryRelocated bool

	// PrimaryExplicit says the primary is launched, probed and read by its
	// dir spelled out — CLAUDE_CONFIG_DIR set to PrimaryDir — never by the
	// variable's absence. Set when HOME is not this user's login home: a
	// second home on one account (an automated pipeline's, say). Spelled out,
	// Claude Code keeps the dir's .claude.json inside it and keys its
	// credential on the dir, exactly as for an extra — which is how such a
	// home has run all along. Absence would read $HOME/.claude.json instead,
	// another file, and key the credential on the base Keychain item, which is
	// per OS user and belongs to the login home's primary. (With HOME
	// overridden, `security` was observed to search no login keychain at all,
	// so today that item is out of reach and logins land in .credentials.json;
	// the spelling does not rest on that.) Claude Code only: Codex keeps its
	// login in the home.
	PrimaryExplicit bool

	// LedgerRoot is the accounts root whose state.json holds the subscription
	// ledger this home spends against: the request ledger, the stored usage
	// responses and the recent launches, all keyed by account identity. ""
	// means this home's own accounts root, as it always was. A second home
	// that holds its own logins of the same subscriptions names the first
	// home's root here (the `.ledger` file beside `.current`), so both homes
	// ask each subscription once per spacing and count each other's launches.
	// What belongs to a home alone — its session re-homes — stays in its own
	// accounts root whatever this says.
	LedgerRoot string
}

// Config holds one Scope per vendor. Load is the only door: every directory
// that crosses into a launch is absolute because it was established here.
type Config struct {
	Home             string
	PrimaryRelocated bool
	Claude, Codex    Scope
}

// Scope returns the vendor's scope; both are always resolved.
func (c Config) Scope(v Vendor) Scope {
	if v == Codex {
		return c.Codex
	}
	return c.Claude
}

// Present lists the scopes of the vendors on this machine, Claude Code first.
func (c Config) Present() []Scope {
	out := make([]Scope, 0, 2)
	for _, v := range Vendors {
		if s := c.Scope(v); s.Present {
			out = append(out, s)
		}
	}
	return out
}

// ForHome resolves both scopes under home with every default and no
// environment override. Load is this plus the overrides and the refusals.
func ForHome(home string) Config {
	c := Config{
		Home: home,
		Claude: Scope{
			Vendor:         Claude,
			Home:           home,
			AccountsRoot:   filepath.Join(home, ".claude-accounts"),
			UsageURL:       "https://api.anthropic.com/api/oauth/usage",
			LauncherFormat: "headroom launch --account %s",
			Spacing:        DefaultSpacing,
			Present:        true,
		},
		Codex: Scope{
			Vendor:         Codex,
			Home:           home,
			AccountsRoot:   filepath.Join(home, ".codex-accounts"),
			UsageURL:       "https://chatgpt.com/backend-api/wham/usage",
			LauncherFormat: "headroom launch --vendor codex --account %s",
			Spacing:        DefaultSpacing,
		},
	}
	return c
}

// HomeScope is the vendor's scope for another home on this machine, as that
// home registered itself in a shared ledger: its home dir, its accounts root
// and how its primary is selected. It locates that home's account dirs so
// their live sessions can be counted; nothing launched is ever built from it.
func HomeScope(v Vendor, home, accountsRoot string, explicit bool) Scope {
	s := ForHome(home).Scope(v)
	s.AccountsRoot = accountsRoot
	if v == Claude {
		s.PrimaryExplicit = explicit
	}
	return s
}

// Load resolves the configuration, refusing relative path overrides outright.
//
// The refusal is load-bearing, not pedantry: verified against the 2.1.220
// binary, a relative CLAUDE_CONFIG_DIR makes claude write its config state
// into a cwd-relative dir while reading credentials from the *default*
// Keychain item — the session runs as the primary under a stray state dir.
// Every config-dir path headroom builds derives from these roots, so
// absoluteness is established here, once, at the only door, for both vendors.
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
			return Config{}, relativeErr("HEADROOM_HOME", v)
		}
		home = v
	}
	c := ForHome(home)
	c.PrimaryRelocated = filepath.Clean(home) != filepath.Clean(realHome)
	c.Claude.PrimaryRelocated, c.Codex.PrimaryRelocated = c.PrimaryRelocated, c.PrimaryRelocated

	for _, o := range []struct {
		scope                          *Scope
		root, name, launcher, usageURL string
	}{
		{&c.Claude, "HEADROOM_ACCOUNTS_ROOT", "HEADROOM_PRIMARY_NAME", "HEADROOM_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL"},
		{&c.Codex, "HEADROOM_CODEX_ACCOUNTS_ROOT", "HEADROOM_CODEX_PRIMARY_NAME", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_CODEX_USAGE_URL"},
	} {
		if v := os.Getenv(o.root); v != "" {
			if !filepath.IsAbs(v) {
				return Config{}, relativeErr(o.root, v)
			}
			o.scope.AccountsRoot = v
		}
		if v := os.Getenv(o.name); v != "" {
			o.scope.PrimaryName = v
		}
		if v := os.Getenv(o.launcher); v != "" {
			if strings.Count(v, "%s") != 1 || strings.Count(v, "%") != 1 {
				return Config{}, fmt.Errorf("%s=%q must contain exactly one %%s (the account name) and no other %% — e.g. \"x-%%s\"", o.launcher, v)
			}
			o.scope.LauncherFormat = v
		}
		if v := os.Getenv(o.usageURL); v != "" {
			o.scope.UsageURL = v
		}
	}

	// Distinct roots are what keep the two vendors' `.current`, `.order` and
	// state.json apart; one location under two names would have each vendor
	// reading the other's routing fact and ledger.
	if sameLocation(c.Claude.AccountsRoot, c.Codex.AccountsRoot) {
		return Config{}, fmt.Errorf("the Claude Code accounts root (%s) and the Codex accounts root (%s) name the same location — each vendor keeps its own .current, .order and state.json, so the two roots must differ", c.Claude.AccountsRoot, c.Codex.AccountsRoot)
	}

	c.Codex.Present = isDir(c.Codex.PrimaryDir()) || isDir(c.Codex.AccountsRoot)

	// A home that is not this user's login home spells its primary out; see
	// Scope.PrimaryExplicit. Unknown login home: today's behavior.
	if login := loginHome(); login != "" && !SameDir(realHome, login) {
		c.Claude.PrimaryExplicit = true
	}
	root, err := readLedger(c.Claude, c.Codex)
	if err != nil {
		return Config{}, err
	}
	c.Claude.LedgerRoot = root
	return c, nil
}

// loginHome is the OS user's own home directory, from the user database —
// never $HOME, which is what a second home overrides. "" when it cannot be
// read. A variable so a test can name one.
var loginHome = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// SameDir reports whether two paths name one directory, by file identity
// when both exist and by spelling otherwise.
func SameDir(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(ai, bi)
}

// readLedger reads the Claude Code scope's `.ledger`, strictly. Absent is
// this home's own ledger. Anything present must pass LedgerRoot; a file that
// does not refuses, the way a relative override does: a home that meant to
// share a ledger and quietly kept its own would ask every subscription twice
// per spacing and pile launches onto accounts the other home is filling —
// silently.
func readLedger(claude, codex Scope) (string, error) {
	path := claude.LedgerFile()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("%s could not be read (%v) — it names the accounts root whose ledger this home shares", path, err)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s is empty — it must name the accounts root whose ledger this home shares, or be deleted", path)
	}
	root, err := LedgerRoot(claude, codex, v)
	if err != nil {
		return "", fmt.Errorf("%s: %v — correct it, or delete it to keep this home's own ledger", path, err)
	}
	return root, nil
}

// LedgerRoot validates v as the accounts root whose ledger a Claude Code home
// shares: one absolute path to an existing directory, other than the Codex
// accounts root. It returns the cleaned path, or "" when v names this home's
// own root — sharing one's own ledger is having one's own.
func LedgerRoot(claude, codex Scope, v string) (string, error) {
	switch {
	case strings.ContainsAny(v, "\n\r"):
		return "", errors.New("a ledger is one accounts root, on one line")
	case !filepath.IsAbs(v):
		return "", fmt.Errorf("%q is not absolute", v)
	case !isDir(v):
		return "", fmt.Errorf("%s is not a directory — it must be the accounts root whose state.json the homes share", v)
	case sameLocation(v, codex.AccountsRoot):
		return "", fmt.Errorf("%s is the Codex accounts root — a Claude Code ledger cannot live there", v)
	case sameLocation(v, claude.AccountsRoot):
		return "", nil
	}
	// A ledger is a place, not a pointer to follow: a root that spends
	// against another root's ledger holds no ledger of its own for this home
	// to share, and two roots naming each other would each spend against the
	// other's file while both read as shared.
	if target := LedgerOf(v); !SameDir(target, v) {
		return "", fmt.Errorf("%s itself spends against %s's ledger — name that root instead", v, target)
	}
	return filepath.Clean(v), nil
}

// LedgerOf is the accounts root whose ledger the home at accountsRoot spends
// against today: what its `.ledger` names, or the root itself when it has
// none. It reads the file and judges nothing — Load is where a home's own
// file is held to the rules; this is how one home asks about another.
func LedgerOf(accountsRoot string) string {
	data, err := os.ReadFile(filepath.Join(accountsRoot, ".ledger"))
	if v := strings.TrimSpace(string(data)); err == nil && v != "" {
		return filepath.Clean(v)
	}
	return filepath.Clean(accountsRoot)
}

func relativeErr(name, v string) error {
	return fmt.Errorf("%s=%q is not absolute — a relative config-dir path would make claude run as the primary while writing state beside the cwd; spell it absolute", name, v)
}

// sameLocation reports whether two roots name one directory. It only
// compares: an accepted spelling is never rewritten.
//
// Identity is the filesystem's, not the string's. Each root is walked up to
// its nearest existing ancestor; the two name one location when those
// ancestors are the same file (so a symlink anywhere above, and a spelling
// that differs only in case on a filesystem that folds it, both count) and
// the components still missing below them match. The missing components are
// compared case-insensitively: whether the filesystem would fold them cannot
// be known before they exist, and refusing a pair of roots that differ only
// by case costs a rename, while accepting one that folds puts both vendors'
// .current and state.json in one directory.
func sameLocation(a, b string) bool {
	aDir, aRest := nearestExisting(filepath.Clean(a))
	bDir, bRest := nearestExisting(filepath.Clean(b))
	if !strings.EqualFold(aRest, bRest) {
		return false
	}
	ai, errA := os.Stat(aDir)
	bi, errB := os.Stat(bDir)
	if errA != nil || errB != nil {
		return aDir == bDir
	}
	return os.SameFile(ai, bi)
}

// nearestExisting splits path into its deepest existing ancestor and the
// components below it that do not exist yet.
func nearestExisting(path string) (dir, rest string) {
	dir = path
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir, rest
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, rest
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// CurrentFile records which account a bare launch targets, per vendor.
// headroom is its only reader (the discovered set's Select, strictly —
// corruption refuses, never defaults) and only writer (the board's enter,
// `launch --remember`).
//
// It is deliberately not part of state.json: one human-legible routing fact
// with a fail-closed policy of its own, kept apart from a ledger that is
// disposable and self-heals by quarantine — neither failure policy should
// govern the other.
func (s Scope) CurrentFile() string { return filepath.Join(s.AccountsRoot, ".current") }

// OrderFile sets dashboard display order after the primary (optional, one
// email per line; unlisted accounts follow alphabetically).
func (s Scope) OrderFile() string { return filepath.Join(s.AccountsRoot, ".order") }

// PrimaryMeta is the .claude.json of the default ~/.claude account, which
// Claude Code keeps at ~/.claude.json — not inside the config dir. Codex
// keeps its identity inside the home, in auth.json.
func (s Scope) PrimaryMeta() string {
	if s.PrimaryExplicit {
		// Spelled out, the primary keeps it inside its dir, like an extra.
		return filepath.Join(s.PrimaryDir(), ".claude.json")
	}
	return filepath.Join(s.Home, ".claude.json")
}

// PrimaryLaunchRefused reports that a primary launch must refuse: the home is
// re-pointed and the primary would be selected by absence, which the vendor
// resolves against the real home. A primary spelled out by its dir names the
// tree the board describes, so it launches.
func (s Scope) PrimaryLaunchRefused() bool { return s.PrimaryRelocated && !s.PrimaryExplicit }

// LedgerFile names the accounts root whose state.json this home's subscription
// ledger lives in (one absolute path on one line). Absent means this home's
// own. Configuration a human or `headroom accounts ledger` writes; read
// strictly by Load.
func (s Scope) LedgerFile() string { return filepath.Join(s.AccountsRoot, ".ledger") }

// Ledger is the accounts root whose state.json holds the subscription ledger.
func (s Scope) Ledger() string {
	if s.LedgerRoot == "" {
		return s.AccountsRoot
	}
	return s.LedgerRoot
}

// SharesLedger reports that the subscription ledger lives in another home's
// accounts root.
func (s Scope) SharesLedger() bool { return s.LedgerRoot != "" && s.LedgerRoot != s.AccountsRoot }

// PrimaryDir is the primary account's state dir. Accounts carry "" for it
// (the vendor's home variable absent), so readers of per-account files
// resolve the real path through here.
func (s Scope) PrimaryDir() string {
	if s.Vendor == Codex {
		return filepath.Join(s.Home, ".codex")
	}
	return filepath.Join(s.Home, ".claude")
}

// StoreLink is the name of the per-account link to the canonical session
// store: Claude Code's projects/, Codex's sessions/.
func (s Scope) StoreLink() string {
	if s.Vendor == Codex {
		return "sessions"
	}
	return "projects"
}

// StoreDir is the vendor's canonical machine-global session store. Every
// extra account's StoreLink symlinks to it, so this one tree is the whole
// session registry. It has no override of its own; it follows HEADROOM_HOME.
func (s Scope) StoreDir() string { return filepath.Join(s.PrimaryDir(), s.StoreLink()) }

// Binary is the executable a launch resolves on PATH and execs as argv[0].
func (s Scope) Binary() string {
	if s.Vendor == Codex {
		return "codex"
	}
	return "claude"
}

// RequestSpacing is Spacing, with the default standing in for a scope built
// without one — a zero spacing must never read as "no quiet period".
func (s Scope) RequestSpacing() time.Duration {
	if s.Spacing <= 0 {
		return DefaultSpacing
	}
	return s.Spacing
}

// Env is the scope's environment policy.
func (s Scope) Env() EnvPolicy { return s.Vendor.EnvPolicy() }
