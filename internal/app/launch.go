package app

// The launch surface: resolving "which account" into a running Claude Code
// process is headroom's job, not the shell's. The wrong-account incident this
// replaces: the zsh wrapper set CLAUDE_CONFIG_DIR for extra accounts but left
// the ambient environment alone for the primary, so a tmux server started
// inside a Claude Code session silently routed every "primary" launch to
// whatever account that session ran on. Here the child environment is
// constructed from the validated decision alone (launch.Target), the
// selection fails closed on corrupt state (accounts.Select), and a
// neutralized ambient value is said out loud — the shell wrappers shrink to
// personal preflight and flags.

import (
	"fmt"
	"os"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
)

// execClaude is the one impure edge of the launch surface, injected so tests
// can capture the argv and environment a launch would have used.
var execClaude = launch.Exec

// target maps a validated account to the launch package's vocabulary through
// the validating constructors: the launch surface never hands the seam a raw
// dir string. The error is impossible for a discovered account — a
// non-primary always carries its real dir — and refusing on it anyway is
// what "impossible" is worth at a routing boundary.
func target(a accounts.Account) (launch.Target, error) {
	if a.IsPrimary() {
		return launch.Primary(), nil
	}
	return launch.Extra(a.ConfigDir)
}

// runResolve prints one line: canonical-name<TAB>config-dir<TAB>kind, kind
// being "primary" or "extra". It exists for shell preflight — topology
// checks need the dir, and *which preflight applies* must come from this
// classification rather than from the shell re-deriving headroom's paths: a
// wrapper that prefix-matches its own idea of the accounts root silently
// skips the check whenever a HEADROOM_* override moves the real one. The
// dir is the primary's real ~/.claude path, never an empty sentinel; the
// launch itself revalidates, so this answer is advice, not a capability.
func runResolve(cfg config.Config, args []string) int {
	selector, explicitEmpty := "", false
	if len(args) > 0 {
		selector, args = args[0], args[1:]
		explicitEmpty = selector == ""
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "headroom resolve: unexpected argument %q\n", args[0])
		return 2
	}
	if explicitEmpty {
		// "" is not a name. Resolving it as "current" would let a caller's
		// expansion bug (`headroom resolve "$broken"`) silently answer with
		// the recorded account; launch refuses the same spelling.
		fmt.Fprintln(os.Stderr, "headroom resolve: an account selector must be non-empty")
		return 2
	}
	a, err := accounts.Select(cfg, accounts.Discover(cfg), selector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom resolve: %v\n", err)
		return 1
	}
	dir := a.Dir(cfg)
	if strings.ContainsAny(a.Name, "\t\n\r") || strings.ContainsAny(dir, "\t\n\r") {
		fmt.Fprintln(os.Stderr, "headroom resolve: account name or dir contains control characters — not launchable")
		return 1
	}
	kind := "extra"
	if a.IsPrimary() {
		kind = "primary"
	}
	fmt.Printf("%s\t%s\t%s\n", a.Name, dir, kind)
	return 0
}

// runLaunch validates the target, optionally records it as where bare `x`
// goes next, constructs the child environment from the decision alone, and
// replaces this process with claude. Everything after `--` goes to claude
// verbatim.
func runLaunch(cfg config.Config, args []string) int {
	remember := false
	account, accountSet := "", false
	rest := []string(nil)
parse:
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--remember":
			remember = true
		case "--account":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom launch: --account needs a value")
				return 2
			}
			i++
			account, accountSet = args[i], true
		case "--":
			rest = args[i+1:]
			break parse
		default:
			fmt.Fprintf(os.Stderr, "headroom launch: unknown argument %q (claude args go after --)\n", args[i])
			return 2
		}
	}
	if accountSet && account == "" {
		// Omitted --account means "the recorded choice"; an explicitly empty
		// one is a malformed name, and collapsing the two would let a
		// caller's expansion bug (`--account "$broken"`) silently launch the
		// recorded account instead of refusing.
		fmt.Fprintln(os.Stderr, "headroom launch: --account needs a non-empty name")
		return 2
	}

	a, err := accounts.Select(cfg, accounts.Discover(cfg), account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
		return 1
	}
	// Both refusals sit before the .current write: a recorded choice that
	// then refuses to launch would move where bare `x` goes as a side effect
	// of a launch that never happened.
	if a.IsPrimary() && cfg.PrimaryRelocated {
		// The primary is selected by CLAUDE_CONFIG_DIR being *absent*, which
		// claude resolves against the real home — HEADROOM_HOME re-points
		// what headroom observes but cannot re-point that. Launching would
		// start a session on a tree the board never described.
		fmt.Fprintln(os.Stderr, "headroom launch: HEADROOM_HOME is set, so the primary headroom describes is not the primary claude would launch — refusing (extras are unaffected)")
		return 1
	}
	if err := accounts.VerifyTopology(cfg, a); err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: not launching — %v\n", err)
		return 1
	}
	if remember {
		// Before the exec, necessarily — and a failure refuses the launch:
		// "chosen and recorded" is one step to the user, and launching on a
		// choice that could not be recorded would have the next bare `x` go
		// somewhere else.
		if err := accounts.SetCurrent(cfg, a.Name); err != nil {
			fmt.Fprintf(os.Stderr, "headroom launch: could not record .current (%v) — not launching\n", err)
			return 1
		}
	}

	tgt, err := target(a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
		return 1
	}
	base := os.Environ()
	if v, conflicting := tgt.Conflicts(base); conflicting {
		// Neutralized either way; said out loud because obeying it would
		// have routed elsewhere. One line, stderr, then business as usual.
		fmt.Fprintf(os.Stderr, "headroom launch: ignoring inherited CLAUDE_CONFIG_DIR=%s; launching %s (%s)\n",
			v, a.Name, a.Dir(cfg))
	}
	if err := execClaude(rest, tgt.Env(base)); err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
		return 1
	}
	return 0
}
