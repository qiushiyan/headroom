package app

// The limits surface: the board's document without the board's costs. A
// machine consumer — a statusline refresher asking about one account a few
// times a minute — cannot afford the board's `--json`, which probes health
// for every account (~170ms per `claude auth status`, the dominant term) and
// may spend every account's request budget to answer about one. This surface
// reads only what is already on disk: discovery, the current marker, and the
// newest stored observation. No health probe, no Keychain read, no claim, no
// request — it is not a second door onto the endpoint because it never
// touches the door at all. Refreshing what is known remains the fetching
// surfaces' job (`headroom --json`, the board, `check`), all behind the same
// claim.

import (
	"fmt"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

func runLimits(cfg config.Config, args []string) int {
	account, accountSet := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--account":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom limits: --account needs a value")
				return 2
			}
			i++
			account, accountSet = args[i], true
		default:
			fmt.Fprintf(os.Stderr, "headroom limits: unknown argument %q\n", args[i])
			return 2
		}
	}
	if accountSet && account == "" {
		// Same policy as launch and resolve: "" is not a name, and treating
		// it as "all accounts" would let a caller's expansion bug silently
		// widen a one-account question.
		fmt.Fprintln(os.Stderr, "headroom limits: --account needs a non-empty name")
		return 2
	}

	accts := accounts.Discover(cfg)
	snap := state.Open(cfg.AccountsRoot).Load()
	now := time.Now()
	list, current := prepareRead(cfg, accts, snap, now)
	if accountSet {
		a, err := accounts.Select(cfg, accts, account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom limits: %v\n", err)
			return 1
		}
		list = filterAccount(list, a.Name)
	}
	data, err := jsonDocument(list, current, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom limits: %v\n", err)
		return 1
	}
	os.Stdout.Write(append(data, '\n'))
	return 0
}

// prepareRead is prepare's network-free half on its own: every account's
// scaffold, with the two skipped questions answered honestly. Health is
// HealthUnprobed — a statement about this surface, never about the account —
// and the attempt axis stays AttemptNone because nothing was attempted; the
// advisory next-eligible instant rides along so a consumer deciding when to
// trigger a real fetch can respect the ledger it will be claimed against.
// (Advice, not permission: the claim alone authorizes traffic.)
func prepareRead(cfg config.Config, accts []accounts.Account, snap state.Snapshot, now time.Time) ([]*accountData, string) {
	// Tolerant, exactly as prepareWith: a corrupt or dangling .current marks
	// nothing as current; repairing it is the board's job, reporting it check's.
	current := ""
	if sel, err := accounts.Select(cfg, accts, ""); err == nil {
		current = sel.Name
	}
	list := make([]*accountData, 0, len(accts))
	for _, a := range accts {
		d := scaffold(cfg, a, current, snap, now)
		d.View.Health = render.HealthUnprobed
		if next := snap.NextEligible(d.Key, now); next.After(now) {
			d.View.Attempt.NextEligibleAt = next.Unix()
		}
		list = append(list, d)
	}
	return list, current
}

func filterAccount(list []*accountData, name string) []*accountData {
	out := make([]*accountData, 0, 1)
	for _, d := range list {
		if d.Acct.Name == name {
			out = append(out, d)
		}
	}
	return out
}
