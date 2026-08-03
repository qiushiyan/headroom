// Package launch owns the one invariant the wrong-account incident proved
// nobody else can be trusted with: a Claude Code child's environment is a
// total function of the validated account decision. Every inherited
// CLAUDE_CONFIG_DIR is removed, and exactly one is added for a non-primary
// account — the primary is selected by the variable being *absent*, the only
// behavior verified against the binary, so an ambient value (a tmux server
// started inside a Claude Code session, a nested shell) can never re-route a
// launch that resolved to a different account.
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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// EnvVar is the vendor's config-dir selector.
const EnvVar = "CLAUDE_CONFIG_DIR"

// Target is a validated routing decision: the primary, or one non-primary
// config dir. The zero value is the primary, so a forgotten construction
// fails toward the account selected by an *absent* variable rather than
// toward an empty-string dir.
type Target struct{ configDir string }

// Primary is the default ~/.claude account, selected by EnvVar being absent.
func Primary() Target { return Target{} }

// Extra is a non-primary account at configDir. An empty dir is refused here,
// at construction: letting it through would build a primary-shaped
// environment under an extra-shaped call. A relative dir is refused for a
// worse reason, verified against the 2.1.220 binary: claude resolves a
// relative CLAUDE_CONFIG_DIR against the cwd for its config files but reads
// credentials from the *default* Keychain item — the child would run as the
// primary while writing its state into a stray dir beside the project, which
// is exactly the misroute this package exists to make unrepresentable.
func Extra(configDir string) (Target, error) {
	if configDir == "" {
		return Target{}, errors.New("launch: an extra account needs a config dir")
	}
	if !filepath.IsAbs(configDir) {
		return Target{}, fmt.Errorf("launch: config dir %q is not absolute — claude would take the primary's credentials from the default Keychain item while writing state beside the cwd", configDir)
	}
	return Target{configDir: configDir}, nil
}

// For maps a dir-or-empty parameter to a Target — the vendor's own
// convention, since CLAUDE_CONFIG_DIR itself means "primary" by absence.
// auth's per-config-dir probe seam is its one production caller; the launch
// surface constructs Primary/Extra explicitly after validation instead. It
// applies Extra's full validation: a probe under a relative dir would answer
// with the default Keychain item's identity — the primary's — and health for
// the wrong account is worse than no answer.
func For(configDir string) (Target, error) {
	if configDir == "" {
		return Target{}, nil
	}
	return Extra(configDir)
}

// IsPrimary reports which variant this is.
func (t Target) IsPrimary() bool { return t.configDir == "" }

// Env builds a child environment for this target from base (normally
// os.Environ()). Every inherited EnvVar entry — duplicates included — is
// removed; a non-primary target's dir is then appended exactly once.
func (t Target) Env(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	if !t.IsPrimary() {
		out = append(out, EnvVar+"="+t.configDir)
	}
	return out
}

// Ambient returns what base carries for EnvVar: its value, and whether the
// variable is present at all. The two must not be conflated — an empty
// present variable is *unverified* vendor territory, not the verified
// absent-means-primary state — so no caller gets a shape that hides the
// difference.
func Ambient(base []string) (value string, present bool) {
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, EnvVar+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Conflicts reports whether the ambient variable, if obeyed instead of
// neutralized, could route somewhere other than this target. For the
// primary any present value conflicts — the primary is selected only by
// absence, and present-but-empty is behavior nobody has verified. For an
// extra, only its exact config dir agrees.
func (t Target) Conflicts(base []string) (value string, conflicting bool) {
	v, present := Ambient(base)
	if !present {
		return "", false
	}
	if t.IsPrimary() {
		return v, true
	}
	return v, v != t.configDir
}

// Exec replaces this process with claude. Replacing rather than spawning is
// what preserves stdio, the controlling terminal, signal delivery and the
// exit status without this tool proxying any of them. It returns only on
// failure.
func Exec(claudeArgs []string, env []string) error {
	path, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	argv := append([]string{"claude"}, claudeArgs...)
	return syscall.Exec(path, argv, env)
}
