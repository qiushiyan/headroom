// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/codexauth"
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
	// --vendor defaults to claude on the commands that act on one account
	// (launch, resolve, accounts add, accounts remove), so every existing
	// invocation means what it meant. The surfaces that report (the board,
	// --json, limits) show every present vendor and the flag restricts them
	// to one. check and sessions take no --vendor.
	var vendor config.Vendor
	vendorSet := false
	switch cmd {
	case "", "accounts", "select", "--json", "limits", "resolve", "launch":
		vendor, vendorSet, rest, err = takeVendor(rest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
			return 2
		}
	}
	// one is the scope a command acting on one account works in.
	one := func() (config.Scope, bool) {
		scope := cfg.Scope(vendor)
		if !scope.Present {
			fmt.Fprintf(os.Stderr, "headroom: %v\n", absentErr(scope))
			return scope, false
		}
		return scope, true
	}
	// reported is the scopes a reporting surface covers.
	reported := func() ([]config.Scope, bool) {
		if !vendorSet {
			return cfg.Present(), true
		}
		scope, ok := one()
		return []config.Scope{scope}, ok
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
				// The one command that works while a vendor is absent: it is
				// how the first Codex directory comes to exist.
				return runAccountsAdd(cfg.Scope(vendor), rest[1:])
			case "remove":
				scope, ok := one()
				if !ok {
					return 1
				}
				return runAccountsRemove(scope, rest[1:])
			}
			layout, rest = boardLayout(rest)
		}
		if !noArgs() {
			return 2
		}
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runAccounts(scopes, layout)
	case "--json":
		if !noArgs() {
			return 2
		}
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runDashboardJSON(scopes)
	case "check", "--check":
		if !noArgs() {
			return 2
		}
		return check.Run(cfg, os.Stdout, stdoutIsTTY())
	case "limits":
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runLimits(scopes, rest)
	case "sessions":
		return runSessions(cfg.Claude, rest)
	case "resolve":
		scope, ok := one()
		if !ok {
			return 1
		}
		return runResolve(scope, rest)
	case "launch":
		scope, ok := one()
		if !ok {
			return 1
		}
		return runLaunch(scope, rest)
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

// takeVendor lifts `--vendor <v>` (or `--vendor=<v>`) out of a command's own
// arguments. It stops at `--`: whatever follows belongs to the launched child,
// and a child's `--vendor` is not headroom's to read.
func takeVendor(args []string) (config.Vendor, bool, []string, error) {
	vendor, set := config.Claude, false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		var value string
		switch {
		case a == "--":
			return vendor, set, append(rest, args[i:]...), nil
		case a == "--vendor":
			if i+1 >= len(args) {
				return "", false, nil, fmt.Errorf("--vendor needs a value: claude or codex")
			}
			i++
			value = args[i]
		case strings.HasPrefix(a, "--vendor="):
			value = strings.TrimPrefix(a, "--vendor=")
		default:
			rest = append(rest, a)
			continue
		}
		v, err := config.ParseVendor(value)
		if err != nil {
			return "", false, nil, err
		}
		vendor, set = v, true
	}
	return vendor, set, rest, nil
}

// absentErr is why a command naming an absent vendor fails. Only Codex can be
// absent.
func absentErr(scope config.Scope) error {
	return fmt.Errorf("%s was not found on this machine (no %s, no %s) — `headroom accounts add --vendor %s <email>` seeds the first account",
		scope.Vendor.Title(), scope.PrimaryDir(), scope.AccountsRoot, scope.Vendor)
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
             With Codex on this machine the board has a page per vendor —
             tab switches, enter records that vendor's account.
             Off a terminal, prints the board once and exits.
  accounts --compact
             the same board, one row per account: percent and time-to-reset
             per limit window, every warning at the end of the row
  accounts add [--vendor <v>] <email> [--share-config[=<dir>]]
             seed the dir for a new subscription: its session store linked
             to the machine-global one (projects/ for claude, sessions/ for
             codex); --share-config links the primary's config (settings,
             skills, …), or every entry of <dir>. Then log in once:
             claude — launch --account <email> and /login;
             codex — launch --vendor codex --account <email> -- login
  accounts remove [<email | name.lock>] [--yes]
             bare on a terminal, pick from the removable accounts; refuse
             while a session is live; delete the account's Keychain item
             and its dir; scrub .order; never touch .current.
             Codex accounts are never removed: headroom cannot tell whether
             a codex session is running on a home
  --json     the board as JSON (schema versioned; every account carries its
             vendor, "current" is keyed by vendor)
  limits     [--account <name>] what is already known about limits, as the
             same JSON document, read from disk alone: no health probe, no
             network — never spends a request. health reads "unprobed"
  sessions   pick any Claude Code session on this machine, enter its project
             dir and continue it on the account that last drove it — execs
             claude in this terminal. --cd-file <abs path> records the
             entered dir for the shell's own cd; claude args go after "--";
             --json lists the sessions instead (no terminal needed)
  launch     [--vendor <v>] [--remember] [--account <name>] [-- <args>]
             exec claude (or codex) on the resolved account; the child
             environment is built from the decision alone, never inherited.
             Codex sessions are shared across its accounts:
             launch --vendor codex --account <other> -- resume [--all]
  resolve    [--vendor <v>] [<name>] print canonical-name<TAB>dir<TAB>kind
             (kind: primary|extra) for shell preflight
  check      verify the reverse-engineered assumptions still hold, for
             every vendor on this machine

  --vendor <claude|codex> defaults to claude on launch, resolve, accounts
  add and accounts remove. accounts, --json and limits show every vendor
  present and take --vendor to show one.
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
func prepare(scope config.Scope, st *state.Store) ([]*accountData, string, state.Snapshot) {
	set := accounts.Discover(scope)
	snap := st.Load()
	src := sources{now: time.Now()}
	if scope.Vendor == config.Claude {
		// Only Claude Code's access costs a process and a Keychain read. The
		// Codex reader works from the auth snapshot discovery already took.
		src.readRaw, src.health = creds.ReadRaw, queryHealthParallel(set.Accounts)
	}
	list, current := prepareWith(set, snap, src)
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
func prepareWith(set accounts.Set, snap state.Snapshot, src sources) ([]*accountData, string) {
	facts, current := accountstate.Assemble(set, snap, src.now)
	list := accountList(facts)
	for _, d := range list {
		// The second of the three places a vendor document is read: access —
		// plan, health, and either a candidate or the attempt state that
		// blocks one.
		if d.Acct.Scope.Vendor == config.Codex {
			codexAccess(d, src.now)
		} else {
			claudeAccess(d, src)
		}
	}
	return list, current
}

func claudeAccess(d *accountData, src sources) {
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

// codexAccess is the Codex health and eligibility table, first match wins.
// There is no vendor probe on this path; the auth snapshot decides. "Relogin
// required" is never produced: the document carries no refresh-token expiry,
// and expiry is read only on positive evidence.
func codexAccess(d *accountData, now time.Time) {
	snap := d.Acct.Auth
	d.View.Plan = snap.Plan
	if d.View.Obs != nil && d.View.Obs.Plan != "" {
		// The plan headroom's own response named outranks the id token's,
		// which is as old as the last login refresh.
		d.View.Plan = d.View.Obs.Plan
	}
	d.View.Attempt = accountstate.Attempt{}
	switch snap.State {
	case codexauth.Absent:
		d.View.Health = accountstate.HealthNoLogin
	case codexauth.Unreadable, codexauth.NoTokens:
		d.View.Health = accountstate.HealthBadBlob
	case codexauth.OtherMode:
		d.View.Health = accountstate.HealthUnknown
		d.View.AuthMode = snap.Mode
	default:
		d.View.Health = accountstate.HealthOK
		d.Request, d.View.Attempt.State = refresh.PrepareCodex(d.Acct, now)
	}
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
func launchFetches(ctx context.Context, list []*accountData, st *state.Store) <-chan refresh.Result {
	requests := make([]*refresh.Candidate, len(list))
	for i, d := range list {
		requests[i] = d.Request
	}
	return refresh.Start(ctx, st, requests, nil)
}

func resolve(d *accountData, result refresh.Result) {
	d.View.Attempt = result.Attempt
	if result.Observation != nil {
		d.View.Obs = result.Observation
		if result.Observation.Plan != "" {
			d.View.Plan = result.Observation.Plan
		}
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
