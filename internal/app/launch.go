package app

// The launch surface: resolving "which account" into a running vendor
// process — Claude Code, or Codex under --vendor codex — is headroom's job,
// not the shell's. The wrong-account incident this replaces: the zsh wrapper
// set CLAUDE_CONFIG_DIR for extra accounts but left the ambient environment
// alone for the primary, so a tmux server started inside a Claude Code session
// silently routed every "primary" launch to whatever account that session ran
// on. Here the child environment is constructed from the validated decision
// alone (launch.Target), a bare launch's routing fails closed on corrupt state
// (set.Bare) as a named one does on an unknown name (set.Select), and a
// neutralized ambient value is said out loud — the shell wrappers shrink to
// personal preflight and flags. This file parses and dispatches; the launch
// itself is one operation the session picker shares (launch_operation.go).

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// execVendor is the one impure edge of the launch surface, injected so tests
// can capture the argv and environment a launch would have used.
var execVendor = launch.ExecPath

// runResolve prints one line: canonical-name<TAB>config-dir<TAB>kind, kind
// being "primary" or "extra". It exists for shell preflight — topology
// checks need the dir, and *which preflight applies* must come from this
// classification rather than from the shell re-deriving headroom's paths: a
// wrapper that prefix-matches its own idea of the accounts root silently
// skips the check whenever a HEADROOM_* override moves the real one. The
// dir is the primary's real ~/.claude path, never an empty sentinel; the
// launch itself revalidates, so this answer is advice, not a capability.
func runResolve(cfg config.Scope, args []string) int {
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
	a, err := accounts.Discover(cfg).Select(selector)
	if errors.Is(err, accounts.ErrAuto) {
		// There is no one account to print. An answer here would be a guess
		// the next launch need not agree with, and a caller launching on it
		// by name would bypass the placement it stood in for.
		dry := "headroom launch --dry-run"
		if cfg.Vendor == config.Codex {
			dry = "headroom launch --vendor codex --dry-run"
		}
		fmt.Fprintf(os.Stderr, "headroom resolve: bare launches are automatic — name an account, or `%s` shows the one a launch would take\n", dry)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom resolve: %v\n", err)
		return 1
	}
	dir := a.Dir()
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

// runLaunch decides the account, records the decision, and replaces this
// process with the vendor. Everything after `--` goes to the vendor verbatim.
//
// The account is decided one of four ways: named (`--account`), the last one
// used (`--last`), pinned (`.current` names it) or automatic (`.current` says
// auto, or `--auto`). Every one of them takes the same path from there — the
// facts gathered for a launch, every account prepared, and placeLaunch, which
// the session picker shares: refuse what is known to refuse, remember, place,
// log — so a named launch is recorded as load the automatic ones can see, says
// where it went like any other, and the child environment is built by
// launch.Prepare from the validated account whichever way it was chosen.
//
// In auto mode every usable account is a correct answer, so nothing about the
// bookkeeping can refuse the launch: a lock that will not come, a section that
// will not decode, a log that will not append each cost a line on stderr.
// Corrupt routing state still refuses, exactly as before — an unreadable
// `.current` says nothing about what the person chose.
func runLaunch(cfg config.Scope, args []string) int {
	var remember, auto, last, dryRun bool
	account, accountSet := "", false
	rest := []string(nil)
parse:
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--remember":
			remember = true
		case "--auto":
			auto = true
		case "--last":
			last = true
		case "--dry-run":
			dryRun = true
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
			fmt.Fprintf(os.Stderr, "headroom launch: unknown argument %q (%s args go after --)\n", args[i], cfg.Binary())
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
	ways := 0
	for _, set := range []bool{auto, last, accountSet} {
		if set {
			ways++
		}
	}
	switch {
	case ways > 1:
		fmt.Fprintln(os.Stderr, "headroom launch: --auto, --last and --account each decide the account — give one")
		return 2
	case dryRun && remember:
		fmt.Fprintln(os.Stderr, "headroom launch: --dry-run records nothing — it cannot be combined with --remember")
		return 2
	case last && remember:
		fmt.Fprintln(os.Stderr, "headroom launch: --last is one launch — to pin an account, --account <name> --remember")
		return 2
	}

	set := accounts.Discover(cfg)
	now := time.Now()
	var intent placement.Intent
	mode := "auto"
	switch {
	case accountSet:
		a, err := set.Select(account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
			return 1
		}
		intent, mode = placement.Intent{Kind: placement.Forced, Account: a.Name, Reason: "named"}, "named"
	case last:
		intent, mode = placement.Intent{Kind: placement.LastUsed}, "last"
	case auto:
	default:
		bare, err := set.Bare()
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
			return 1
		}
		if !bare.Auto {
			intent, mode = placement.Intent{Kind: placement.Forced, Account: bare.Account.Name, Reason: "pinned"}, "pinned"
		}
	}
	automatic := intent.Kind == placement.Auto

	cwd, _ := os.Getwd()
	plan := launchop.Gather(launchop.Request{
		Set: set, Store: state.Open(cfg), Intent: intent, Mode: mode, VendorArgs: rest,
		Remember: remember, CWD: cwd, Environ: os.Environ(), Now: now,
	}, placeDeps())
	if dryRun {
		// What the launch would refuse before writing anything is said as the
		// launch says it; what it would decide is the table.
		chosen, err := plan.Preview(os.Stdout, cfg.Binary())
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
			return 1
		}
		if !chosen {
			return 1
		}
		return 0
	}

	// --remember is written by the operation, after what it would refuse and
	// before the placement and the exec.
	out, err := plan.Commit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: %v\n", err)
		return 1
	}

	out.Announce(os.Stderr, "headroom launch: ")
	p := out.Prepared()
	if err := execVendor(p.Path, p.Binary, rest, p.Env); err != nil {
		fmt.Fprintf(os.Stderr, "headroom launch: exec %s: %v\n", p.Binary, err)
		if remember {
			current := out.Account().Name
			if automatic {
				current = accounts.AutoWord
			}
			fmt.Fprintf(os.Stderr, "headroom launch: .current remains %s\n", current)
		}
		return 1
	}

	return 0
}
