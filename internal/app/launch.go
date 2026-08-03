package app

// The launch surface: resolving "which account" into a running Claude Code
// process is headroom's job, not the shell's. The wrong-account incident this
// replaces: the zsh wrapper set CLAUDE_CONFIG_DIR for extra accounts but left
// the ambient environment alone for the primary, so a tmux server started
// inside a Claude Code session silently routed every "primary" launch to
// whatever account that session ran on. Here the child environment is
// constructed from the validated decision alone (launch.Env), the selection
// fails closed on corrupt state (accounts.Select), and a neutralized ambient
// value is said out loud — the shell wrappers shrink to personal preflight
// and flags.

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

// runResolve prints one line: canonical-account-name<TAB>config-dir (the
// primary's real ~/.claude path, not the empty internal sentinel). It exists
// for shell preflight — seeding, topology checks — which needs the dir before
// a launch; the launch itself revalidates, so this answer is advice, not a
// capability.
func runResolve(cfg config.Config, args []string) int {
	selector := ""
	if len(args) > 0 {
		selector, args = args[0], args[1:]
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "headroom resolve: unexpected argument %q\n", args[0])
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
	fmt.Printf("%s\t%s\n", a.Name, dir)
	return 0
}

// runLaunch validates the target, optionally records it as where bare `x`
// goes next, constructs the child environment from the decision alone, and
// replaces this process with claude. Everything after `--` goes to claude
// verbatim.
func runLaunch(cfg config.Config, args []string) int {
	remember := false
	account := ""
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
			account = args[i]
		case "--":
			rest = args[i+1:]
			break parse
		default:
			fmt.Fprintf(os.Stderr, "headroom launch: unknown argument %q (claude args go after --)\n", args[i])
			return 2
		}
	}

	a, err := accounts.Select(cfg, accounts.Discover(cfg), account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
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

	base := os.Environ()
	if inherited := launch.Inherited(base); inherited != "" && inherited != a.Dir(cfg) {
		// Neutralized either way; said out loud only when it would have
		// re-routed the launch. One line, stderr, then business as usual.
		fmt.Fprintf(os.Stderr, "headroom launch: ignoring inherited CLAUDE_CONFIG_DIR=%s; launching %s (%s)\n",
			inherited, a.Name, a.Dir(cfg))
	}
	if err := execClaude(rest, launch.Env(base, a.ConfigDir)); err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
		return 1
	}
	return 0
}
