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
	"io"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

func runLimits(cfg config.Scope, args []string) int {
	return runLimitsTo(os.Stdout, cfg, args)
}

// runLimitsTo is runLimits with the document's destination injected, so the
// command — flag policy included — is testable without capturing the process's
// stdout. Diagnostics still go to stderr: the document stream carries JSON or
// nothing.
func runLimitsTo(w io.Writer, cfg config.Scope, args []string) int {
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

	now := time.Now()
	disk := accountstate.Read(cfg, state.Open(cfg), now)
	list, current, snap := accountList(disk.Accounts), disk.Current, disk.Store
	if accountSet {
		a, err := setOf(cfg, list).Select(account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom limits: %v\n", err)
			return 1
		}
		list = filterAccount(list, a.Name)
	}
	data, err := jsonDocument(list, current, snap.Problems(), now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom limits: %v\n", err)
		return 1
	}
	w.Write(append(data, '\n'))
	return 0
}

func accountList(facts []accountstate.Account) []*accountData {
	list := make([]*accountData, 0, len(facts))
	for _, a := range facts {
		list = append(list, &accountData{Acct: a.Acct, Key: a.Key, View: a.View})
	}
	return list
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
