// Package launch owns the one invariant the wrong-account incident proved
// nobody else can be trusted with: a Claude Code child's environment is a
// total function of the validated account decision. Every inherited
// CLAUDE_CONFIG_DIR is removed, and exactly one is added for a non-primary
// account — the primary is selected by the variable being *absent*, the only
// behavior verified against the binary, so an ambient value (a tmux server
// started inside a Claude Code session, a nested shell) can never re-route a
// launch that resolved to a different account.
//
// Env is the single environment constructor: auth's health probes and the
// launch command both build their child environments here, so subprocess
// routing has one implementation to be right.
package launch

import (
	"os/exec"
	"strings"
	"syscall"
)

// EnvVar is the vendor's config-dir selector.
const EnvVar = "CLAUDE_CONFIG_DIR"

// Env builds a child environment for one account from base (normally
// os.Environ()). configDir "" means the primary: the variable is stripped and
// not re-added. A non-primary dir is appended exactly once, after every
// inherited copy — duplicates included — is removed.
func Env(base []string, configDir string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	if configDir != "" {
		out = append(out, EnvVar+"="+configDir)
	}
	return out
}

// Inherited returns the value base carries for EnvVar, "" when absent. It
// reads the same slice Env consumes so a caller warning about a neutralized
// value and the construction that neutralizes it cannot disagree.
func Inherited(base []string) string {
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, EnvVar+"="); ok {
			return v
		}
	}
	return ""
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
