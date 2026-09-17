// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

func Run(args []string) int {
	cmd := ""
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	// retired 2026-08-04: see DESIGN.md § The session surface. The tombstone
	// dispatches before configuration on purpose — the shell it diagnoses is
	// stale, and stale shells come with old environments that Load may now
	// refuse; this message must speak regardless. It stays indefinitely
	// ("no shell still runs the old wrapper" is unobservable) and the name
	// must never be rebound: its stdout was a decision protocol, and one
	// name with two meanings across binary generations is the incident class
	// this arm exists to close. Nothing parseable goes to stdout.
	if cmd == "resume" {
		fmt.Fprint(os.Stderr, `headroom: `+"`resume`"+` was retired. Its stdout was a decision protocol, and a
shell function loaded before this binary can misread it — that produced a
session on the wrong account.
Your shell integration is stale. Fix it:  exec zsh
The session picker is now `+"`headroom sessions`"+` (listing: `+"`headroom sessions --json`"+`).
`)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
		return 2
	}
	// Commands without options reject any remaining arguments.
	noArgs := func() bool {
		if len(rest) == 0 {
			return true
		}
		fmt.Fprintf(os.Stderr, "headroom: unexpected argument %q\n", rest[0])
		printUsage(os.Stderr)
		return false
	}
	switch cmd {
	// "select" is what this surface was called before it became the whole
	// board; accepted so a shell integration mid-update keeps working.
	case "", "accounts", "select":
		// The board's noun also carries the two commands that change the
		// account set, and the board's one presentation flag; the bare
		// invocation and the compatibility spelling still take nothing.
		layout := render.LayoutBlocks
		if cmd == "accounts" && len(rest) > 0 {
			switch rest[0] {
			case "add":
				return runAccountsAdd(cfg, rest[1:])
			case "remove":
				return runAccountsRemove(cfg, rest[1:])
			}
			layout, rest = boardLayout(rest)
		}
		if !noArgs() {
			return 2
		}
		return runAccounts(cfg, layout)
	case "--json":
		if !noArgs() {
			return 2
		}
		return runDashboardJSON(cfg)
	case "check", "--check":
		if !noArgs() {
			return 2
		}
		return check.Run(cfg, os.Stdout, stdoutIsTTY())
	case "limits":
		return runLimits(cfg, rest)
	case "sessions":
		return runSessions(cfg, rest)
	case "resolve":
		return runResolve(cfg, rest)
	case "launch":
		return runLaunch(cfg, rest)
	case "-h", "--help", "help":
		if !noArgs() {
			return 2
		}
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "headroom: unknown command %q\n", cmd)
		printUsage(os.Stderr)
		return 2
	}
}

// boardLayout reads the board's one presentation flag off the front of its
// arguments. Absent, the layout is the blocks — the default is the default
// whatever else is typed — and whatever follows the flag is left for the
// no-arguments check to refuse.
func boardLayout(args []string) (render.Layout, []string) {
	if len(args) > 0 && args[0] == "--compact" {
		return render.LayoutCompact, args[1:]
	}
	return render.LayoutBlocks, args
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: headroom [command]

  (none)     the account board: live usage for every account, refreshing
  accounts   while it is open; enter picks the account a bare launch targets.
             Off a terminal, prints the board once and exits.
  accounts --compact
             the same board, one row per account: percent and time-to-reset
             per limit window, every warning at the end of the row
  accounts add <email> [--share-config[=<dir>]]
             seed the dir for a new subscription: projects/ linked to the
             machine-global store; --share-config links the primary's
             config (settings, skills, commands, hooks, …), or every entry
             of <dir>. Then: launch --account <email> and /login once
  accounts remove [<email | name.lock>] [--yes]
             bare on a terminal, pick from the removable accounts; refuse
             while a session is live; delete the account's Keychain item
             and its dir; scrub .order; never touch .current
  --json     the board as JSON (schema versioned)
  limits     [--account <name>] what is already known about limits, as the
             same JSON document, read from disk alone: no health probe, no
             network — never spends a request. health reads "unprobed"
  sessions   pick any session on this machine, enter its project dir and
             continue it on the account that last drove it — execs claude
             in this terminal. --cd-file <abs path> records the entered
             dir for the shell's own cd; claude args go after "--";
             --json lists the sessions instead (no terminal needed)
  launch     [--remember] [--account <name>] [-- <claude args>]
             exec claude on the resolved account; the child environment is
             built from the decision alone, never inherited
  resolve    [<name>] print canonical-name<TAB>config-dir<TAB>kind
             (kind: primary|extra) for shell preflight
  check      verify the reverse-engineered assumptions still hold
`)
}

func stdoutIsTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

type accountData struct {
	Acct    accounts.Account
	Key     state.Key
	View    accountstate.Facts
	Request *refresh.Candidate
}

// sources are the three inputs prepare reads about each account, injected so
// the pipeline can be table-tested without a Keychain, a Claude Code install,
// or a network.
type sources struct {
	readRaw func(configDir string) string
	health  auth.QueryFunc
	now     time.Time
}

// prepare walks accounts and assembles what is known about each one before
// any request goes out: whether Claude Code considers it usable, the newest
// quota figures already on disk, and whether a live refresh is affordable.
// It also returns the current-target name it marked the views with —
// consumers that report the current account must use this value, not re-read
// the state file, or a concurrent `select` could make the two disagree.
func prepare(cfg config.Config, st *state.Store) ([]*accountData, string, state.Snapshot) {
	accts := accounts.Discover(cfg)
	snap := st.Load()
	list, current := prepareWith(cfg, accts, snap, sources{
		readRaw: creds.ReadRaw,
		health:  queryHealthParallel(accts),
		now:     time.Now(),
	})
	return list, current, snap
}

// queryHealthParallel runs `claude auth status` for every account at once and
// hands back a lookup. Serially this would cost one process spawn per account
// before anything renders; in parallel it is one spawn's latency total.
func queryHealthParallel(accts []accounts.Account) auth.QueryFunc {
	results := make([]auth.Status, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			results[i] = auth.Query(dir)
		}(i, a.ConfigDir)
	}
	wg.Wait()
	byDir := make(map[string]auth.Status, len(accts))
	for i, a := range accts {
		byDir[a.ConfigDir] = results[i]
	}
	return func(configDir string) auth.Status { return byDir[configDir] }
}

// prepareWith is prepare with its inputs injected.
func prepareWith(cfg config.Config, accts []accounts.Account, snap state.Snapshot, src sources) ([]*accountData, string) {
	facts, current := accountstate.Assemble(cfg, accts, snap, src.now)
	list := accountList(facts)
	for _, d := range list {
		raw := src.readRaw(d.Acct.ConfigDir)
		blob, ok := creds.Parse(raw)
		if ok {
			d.View.Plan = blob.PlanLabel()
		}
		d.View.Health = resolveHealth(src.health(d.Acct.ConfigDir), raw, blob, ok, src.now.UnixMilli())
		d.View.Attempt = accountstate.Attempt{}
		if d.View.Health == accountstate.HealthOK {
			d.Request, d.View.Attempt.State = refresh.Prepare(d.Acct, blob, ok, src.now)
		}
	}
	return list, current
}

// resolveHealth decides one thing: can Claude Code use this account.
//
// Claude Code's own verdict settles it whenever there is one. A credential
// headroom cannot read blocks *headroom's* request, not the account — that
// belongs on the attempt axis, and letting it decide health here would
// recreate the false alarm this rework exists to remove. Credential evidence
// is the fallback only when the oracle has no answer, and it never infers
// "expired" from a missing field (see creds.ReloginRequired).
func resolveHealth(st auth.Status, raw string, blob creds.Blob, blobOK bool, nowMS int64) accountstate.Health {
	switch st.Outcome {
	case auth.OutcomeOK:
		if st.LoggedIn {
			return accountstate.HealthOK
		}
		return accountstate.HealthNoLogin
	case auth.OutcomeUnparseable:
		// The oracle ran and answered in a shape we no longer understand.
		// Guessing from credentials here would paper over vendor drift.
		return accountstate.HealthUnknown
	case auth.OutcomeUnrunnable:
		// The probe's environment could not be built (launch refused the
		// dir). The credential fallback below reads the same broken spelling
		// and would report "no login" — a /login errand for a path bug.
		return accountstate.HealthUnknown
	}
	switch {
	case raw == "":
		return accountstate.HealthNoLogin
	case !blobOK:
		return accountstate.HealthBadBlob
	case blob.ReloginRequired(nowMS):
		return accountstate.HealthReloginRequired
	default:
		return accountstate.HealthOK
	}
}

// Each round owns its channel and list until the channel has closed and drained.
func launchFetches(ctx context.Context, cfg config.Config, list []*accountData, st *state.Store) <-chan refresh.Result {
	requests := make([]*refresh.Candidate, len(list))
	for i, d := range list {
		requests[i] = d.Request
	}
	return refresh.Start(ctx, cfg.UsageURL, st, requests, nil)
}

func resolve(d *accountData, result refresh.Result) {
	d.View.Attempt = result.Attempt
	if result.Observation != nil {
		d.View.Obs = result.Observation
	}
	if result.StoreErr != nil {
		d.View.Attempt.StoreError = result.StoreErr.Error()
	}
}

func views(list []*accountData) []accountstate.Facts {
	vs := make([]accountstate.Facts, len(list))
	for i, d := range list {
		vs[i] = d.View
	}
	return vs
}
