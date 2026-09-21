// Package launch owns the one invariant the wrong-account incident proved
// nobody else can be trusted with: a launched child's environment is a total
// function of the validated account decision. Every inherited value of the
// vendor's home variable (CLAUDE_CONFIG_DIR, CODEX_HOME) is removed, and
// exactly one is added for a non-primary account — the primary is selected by
// the variable being *absent*, the only behavior verified against the
// binaries, so an ambient value (a tmux server started inside a managed
// session, a nested shell) can never re-route a launch that resolved to a
// different account. The variables are the vendor's row of one policy table
// (config.EnvPolicy); this package is the one constructor that applies it.
//
// The decision crosses this package's interface only as a Target, built by
// one of two constructors — the empty-string-means-primary sentinel that
// account discovery still carries internally cannot cross it, so "primary
// plus a config dir" and "extra with no dir" are unrepresentable here.
// Env is the single environment constructor and Conflicts the single
// conflict classifier: auth's health probes, the launch command, the board's
// note and check's report all answer through them, so subprocess routing and
// its diagnostics cannot disagree.
package launch

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qiushiyan/headroom/internal/config"
)

// The variables themselves are data on the vendor's environment policy
// (config.EnvPolicy): the home variable that selects a non-primary account,
// and every inherited variable the constructor strips because obeying it
// would route or authenticate the child some other way.
//
// For Claude Code the second stripped variable, CLAUDE_SECURESTORAGE_CONFIG_DIR,
// redirects the vendor's *credential* lookup independently of the config dir
// (verified 2.1.220 by experiment: with it set, `auth status` answers from
// the named dir's Keychain item while config state stays the config dir's).
// An ambient value would therefore split any managed launch into a chimera —
// one account's tokens under another's state. For Codex, CODEX_SQLITE_HOME
// re-points the session index and CODEX_API_KEY / CODEX_ACCESS_TOKEN are read
// ahead of the home's auth.json (the latter measured on 0.155.0), so an
// inherited one would run the chosen home on another login. headroom never
// sets any of them.

// Target is a validated routing decision: one vendor's primary, or one
// non-primary dir of that vendor. The zero value is Claude Code's primary, so
// a forgotten construction fails toward the account selected by an *absent*
// variable rather than toward an empty-string dir.
type Target struct {
	vendor    config.Vendor
	configDir string
}

// Primary is the vendor's default account, selected by its home variable
// being absent.
func Primary(v config.Vendor) Target { return Target{vendor: v} }

// Extra is a non-primary account at configDir. An empty dir is refused here,
// at construction: letting it through would build a primary-shaped
// environment under an extra-shaped call. A relative dir is refused for a
// worse reason, verified against the 2.1.220 binary: claude resolves a
// relative CLAUDE_CONFIG_DIR against the cwd for its config files but reads
// credentials from the *default* Keychain item — the child would run as the
// primary while writing its state into a stray dir beside the project, which
// is exactly the misroute this package exists to make unrepresentable. Codex
// canonicalizes a relative CODEX_HOME against the cwd (measured 0.155.0),
// which is a different misroute and the same refusal.
func Extra(v config.Vendor, configDir string) (Target, error) {
	if configDir == "" {
		return Target{}, errors.New("launch: an extra account needs a config dir")
	}
	if !filepath.IsAbs(configDir) {
		return Target{}, fmt.Errorf("launch: config dir %q is not absolute — claude would take the primary's credentials from the default Keychain item while writing state beside the cwd", configDir)
	}
	return Target{vendor: v, configDir: configDir}, nil
}

// For maps a dir-or-empty parameter to a Target — the vendors' own
// convention, since the home variable itself means "primary" by absence.
// It applies Extra's full validation: a probe under a relative dir would
// answer with the default Keychain item's identity — the primary's — and
// health for the wrong account is worse than no answer.
func For(v config.Vendor, configDir string) (Target, error) {
	if configDir == "" {
		return Primary(v), nil
	}
	return Extra(v, configDir)
}

// IsPrimary reports which variant this is.
func (t Target) IsPrimary() bool { return t.configDir == "" }

// Env builds a child environment for this target from base (normally
// os.Environ()). Every inherited entry of every variable the vendor's policy
// strips — duplicates included — is removed; a non-primary target's dir is
// then appended exactly once. Only the target's own vendor's variables are
// touched: a Codex launch leaves Claude Code's alone, and the reverse.
func (t Target) Env(base []string) []string {
	policy := t.vendor.EnvPolicy()
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !stripped(policy, kv) {
			out = append(out, kv)
		}
	}
	if !t.IsPrimary() {
		out = append(out, policy.HomeVar+"="+t.configDir)
	}
	return out
}

func stripped(policy config.EnvPolicy, kv string) bool {
	for _, name := range policy.Strip {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// Inherited is one variable the constructor removed from the environment.
type Inherited struct {
	Name, Value string
	Secret      bool
}

// String names the variable, and its value unless it is a credential: a
// notice must never carry a token into a terminal, a log or a bug report.
func (i Inherited) String() string {
	if i.Secret {
		return i.Name + " (value not shown)"
	}
	return i.Name + "=" + i.Value
}

// Redirects lists every inherited variable the vendor's policy strips other
// than the home variable, which has a classifier of its own (Conflicts).
// Always worth saying when present: headroom never produces one, and obeying
// it would take credentials or state from somewhere the routing decision
// never named.
func Redirects(v config.Vendor, base []string) []Inherited {
	policy := v.EnvPolicy()
	var out []Inherited
	for _, name := range policy.Strip {
		if name == policy.HomeVar {
			continue
		}
		for _, kv := range base {
			if val, ok := strings.CutPrefix(kv, name+"="); ok {
				out = append(out, Inherited{Name: name, Value: val, Secret: policy.IsSecret(name)})
				break
			}
		}
	}
	return out
}

// Ambient returns what base carries for the vendor's home variable: its
// value, and whether the variable is present at all. The two must not be
// conflated — an empty present variable is *unverified* vendor territory,
// not the verified absent-means-primary state — so no caller gets a shape
// that hides the difference.
func Ambient(v config.Vendor, base []string) (value string, present bool) {
	name := v.EnvPolicy().HomeVar
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Conflicts reports whether the ambient home variable, if obeyed instead of
// neutralized, could route somewhere other than this target. For the
// primary any present value conflicts — the primary is selected only by
// absence, and present-but-empty is behavior nobody has verified. For an
// extra, only its exact config dir agrees.
func (t Target) Conflicts(base []string) (value string, conflicting bool) {
	v, present := Ambient(t.vendor, base)
	if !present {
		return "", false
	}
	if t.IsPrimary() {
		return v, true
	}
	return v, v != t.configDir
}

// ExecPath replaces this process with the executable already resolved, under
// argv[0] = binary. A caller that chdirs before exec'ing resolves first, from
// the directory the user invoked in — otherwise a relative PATH entry could
// supply a project-local binary from whatever directory was just entered.
func ExecPath(path, binary string, args []string, env []string) error {
	argv := append([]string{binary}, args...)
	return syscall.Exec(path, argv, env)
}
