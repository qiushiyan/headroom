package app

// The launch surface: resolving "which account" into a running vendor
// process — Claude Code, or Codex under --vendor codex — is headroom's job,
// not the shell's. The wrong-account incident this replaces: the zsh wrapper
// set CLAUDE_CONFIG_DIR for extra accounts but left the ambient environment
// alone for the primary, so a tmux server started inside a Claude Code session
// silently routed every "primary" launch to whatever account that session ran
// on. Here the child environment is constructed from the validated decision
// alone (launch.Target), a named launch fails closed on an unknown name
// (set.Select), and a neutralized ambient value is said out loud — the shell
// wrappers shrink to personal preflight and flags. This file parses and
// dispatches; the launch itself is one operation the session picker shares
// (launchop).

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// Resolve prints one line: canonical-name<TAB>config-dir<TAB>kind, kind
// being "primary" or "extra", for the account name names. It exists for
// shell preflight — topology checks need the
// dir, and *which preflight applies* must come from this classification
// rather than from the shell re-deriving headroom's paths: a wrapper that
// prefix-matches its own idea of the accounts root silently skips the check
// whenever a HEADROOM_* override moves the real one. The dir is the
// primary's real ~/.claude path, never an empty sentinel; the launch itself
// revalidates, so this answer is advice, not a capability.
func Resolve(std Streams, scope config.Scope, name string) error {
	a, err := accounts.Discover(scope).Select(name)
	if err != nil {
		return err
	}
	dir := a.Dir()
	if strings.ContainsAny(a.Name, "\t\n\r") || strings.ContainsAny(dir, "\t\n\r") {
		return errors.New("account name or dir contains control characters — not launchable")
	}
	kind := "extra"
	if a.IsPrimary() {
		kind = "primary"
	}
	fmt.Fprintf(std.Out, "%s\t%s\t%s\n", a.Name, dir, kind)
	return nil
}

// LaunchOptions are how a launch decides its account and what it hands the
// vendor. At most one of Account and Last decides; neither means the rule
// chooses.
type LaunchOptions struct {
	Account string // the account named; "" for none
	Last    bool
	DryRun  bool
	// Args reach the vendor exactly as given.
	Args []string
}

// Launch decides the account, records the decision, and replaces this
// process with the vendor.
//
// The account is decided one of three ways: named, the last one used, or by
// the placement rule. Every one of them takes the same path from there —
// launchop gathers what a launch reads, refuses what is known to refuse,
// places and logs — so a named launch is recorded as load the automatic ones
// can see, says where it went like any other, and the child environment is
// built by launch.Prepare from the validated account whichever way it was
// chosen.
//
// For an automatic launch every usable account is a correct answer, so
// nothing about the bookkeeping can refuse it: a lock that will not come, a
// section that will not decode, a log that will not append each cost a line
// on stderr. A reservation that cannot be read still refuses — it says
// nothing about which accounts are kept for what.
func Launch(std Streams, scope config.Scope, opts LaunchOptions, place launchop.Deps, exec ExecFunc) error {
	set := accounts.Discover(scope)
	now := time.Now()
	var intent placement.Intent
	mode := "auto"
	switch {
	case opts.Account != "":
		a, err := set.Select(opts.Account)
		if err != nil {
			return err
		}
		intent, mode = placement.Intent{Kind: placement.Forced, Account: a.Name, Reason: "named"}, "named"
	case opts.Last:
		intent, mode = placement.Intent{Kind: placement.LastUsed}, "last"
	}

	cwd, _ := os.Getwd()
	plan := launchop.Gather(launchop.Request{
		Set: set, Store: state.Open(scope), Intent: intent, Mode: mode, VendorArgs: opts.Args,
		CWD: cwd, Environ: os.Environ(), Now: now,
	}, place)
	if opts.DryRun {
		// What the launch would refuse before writing anything is said as the
		// launch says it; what it would decide is the table.
		chosen, err := plan.Preview(std.Out, scope.Binary())
		if err != nil {
			return err
		}
		if !chosen {
			return Status(1)
		}
		return nil
	}

	out, err := plan.Commit()
	if err != nil {
		return err
	}
	out.Announce(std.Err, "headroom launch: ")
	p := out.Prepared()
	if err := exec(p.Path, p.Binary, opts.Args, p.Env); err != nil {
		return fmt.Errorf("exec %s: %v%s", p.Binary, err, out.Stands())
	}
	return nil
}
