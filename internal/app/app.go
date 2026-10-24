// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/round"
)

func Run(args []string) int {
	cmd := ""
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	// headroom as a vendor's $BROWSER, set by `login` and `headless mint`
	// (approval.go): record the
	// sign-in the one URL belongs to, open it in the profile named if one is,
	// and do nothing else. Before configuration, since the vendor's
	// environment is not one headroom needs to accept.
	if browser.IsOpenRequest(args) {
		profileDir, open := os.LookupEnv(browser.ProfileEnv)
		signin, record := os.LookupEnv(browser.SigninEnv)
		if open || record {
			return runOpen(args[0], signin, profileDir, open)
		}
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
	if cmd == "version" || cmd == "--version" {
		// Before configuration, like the tombstone: what binary this is must
		// be answerable whatever the environment or the files say.
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "headroom: unexpected argument %q\n", rest[0])
			return 2
		}
		fmt.Println(versionLine())
		return 0
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
		if cmd == "check" || cmd == "--check" {
			// A configuration headroom refuses is not an assertion it could
			// not test: it is its own state, broken, and every launch is
			// refusing on it right now.
			fmt.Fprintln(os.Stdout, "FAIL  config: headroom refuses its configuration — every command refuses until it is fixed")
			return check.ExitFail
		}
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
	case "", "accounts", "select", "--json", "limits", "resolve", "launch", "launches", "refresh":
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
			case "ledger":
				if vendor != config.Claude {
					fmt.Fprintln(os.Stderr, "headroom accounts ledger: only Claude Code homes share a ledger")
					return 2
				}
				return runAccountsLedger(cfg, rest[1:])
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
		return check.Run(cfg, os.Stdout, stdoutIsTTY(), pidProbe)
	case "limits":
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runLimits(scopes, rest)
	case "sessions":
		return runSessions(cfg.Claude, rest)
	case "login":
		return runLogin(cfg.Claude, rest)
	case "headless":
		return runHeadless(cfg.Claude, rest)
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
	case "launches":
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runLaunches(scopes, rest)
	case "refresh":
		if !noArgs() {
			return 2
		}
		scopes, ok := reported()
		if !ok {
			return 1
		}
		return runRefresh(scopes)
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

// version is set at link time when a build names itself
// (-ldflags "-X github.com/qiushiyan/headroom/internal/app.version=<v>");
// otherwise the commit the binary was built from names it.
var version = ""

// versionLine is `headroom version`: one line, the build's name — the commit
// it was built from, "+dirty" when the tree had uncommitted changes — and the
// commit's time. What a caller pins.
func versionLine() string {
	rev, at, dirty := "", "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	v := version
	if v == "" {
		v = rev
		if v == "" {
			v = "unknown"
		}
		if dirty {
			v += "+dirty"
		}
	}
	line := "headroom " + v
	if at != "" {
		line += " " + at
	}
	return line
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
  accounts   while it is open; enter pins the account a bare launch targets,
             and a turns automatic placement on instead: each bare launch
             then chooses the least-loaded account, marked "next" here.
             With Codex on this machine the board has a page per vendor —
             tab switches, enter and a record that vendor's routing.
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
  accounts ledger [<accounts root>]
             with a root: this home spends against that root's ledger —
             a second home on this machine holding logins of the same
             subscriptions; both then ask each subscription once per
             spacing and count each other's sessions and launches.
             Naming this home's own root stops sharing. Either way it
             registers this home there and lists the homes registered
  accounts remove [<email | name.lock>] [--yes]
             bare on a terminal, pick from the removable accounts; refuse
             while a session is live; delete the account's Keychain item
             and its dir; scrub .order; never touch .current.
             Codex accounts are never removed: headroom cannot tell whether
             a codex session is running on a home
  login      [<name>…] [--all] [--within <days>] [--dry-run]
             renew Claude Code logins: each one that ends within 7 days
             (or --within), is ended or never logged in — or the named
             ones, or --all. One claude auth login per account, in turn;
             its approval page opens in the Chrome profile matched to the
             account's email, and the result is read back. --dry-run
             prints the plan only
  --json     the board as JSON (schema versioned; every account carries its
             vendor, "current" and "mode" are keyed by vendor)
  limits     [--account <name>] what is already known about limits, as the
             same JSON document, read from disk alone: no health probe, no
             network — never spends a request. health reads "unprobed"
  sessions   pick any Claude Code session on this machine, enter its project
             dir and continue it on the account that last drove it — execs
             claude in this terminal. --cd-file <abs path> records the
             entered dir for the shell's own cd; claude args go after "--";
             --json lists the sessions instead (no terminal needed)
  launch     [--vendor <v>] [--auto | --last | --account <name>]
             [--remember] [--dry-run] [-- <args>]
             exec claude (or codex) on the decided account; the child
             environment is built from the decision alone, never inherited.
             Bare, it follows the board: the pinned account, or under auto
             the least-loaded one — said on stderr before the vendor starts.
             --auto places this one launch automatically, --last reuses the
             last account used, --account names one. --remember records the
             account (or, with --auto, the mode) for later bare launches.
             --dry-run prints the choice and every account's figures, and
             records, logs and starts nothing.
             Under auto, --resume <id> and --session-id <id> follow the
             session's account while it is not near a limit.
             Codex sessions are shared across its accounts:
             launch --vendor codex --account <other> -- resume [--all]
  launches   [-n <count>] [--json] the newest launches, one line each: when,
             which account, how it was decided, and the runner-up
  launches --eval [--since <7d | 2026-10-01>] [--json]
             how automatic placement has done, per rule version: windows
             its launches went into that reached a limit while another
             account had room, weekly room that may have renewed unspent,
             how the chosen account's session window went after each
             launch, and the launches this binary's rule would decide
             differently. Observed outcomes, not what another choice would
             have caused. Reads the launch logs and the usage log; asks
             nothing
  refresh    ask the usage endpoint about every account that may be asked
             and store the answers; prints nothing. An automatic launch
             leaves one running for the next launch
  resolve    [--vendor <v>] [<name>] print canonical-name<TAB>dir<TAB>kind
             (kind: primary|extra) for shell preflight; under auto a name
             is required
  headless   mint|place|started|token|forget — headless sessions on setup-tokens
  check      verify the reverse-engineered assumptions still hold, for
             every vendor on this machine
  version    the commit this binary was built from, and its time

  --vendor <claude|codex> defaults to claude on launch, resolve, accounts
  add and accounts remove. accounts, --json, limits, launches and refresh
  cover every vendor present and take --vendor for one.
`)
}

func stdoutIsTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// roundDeps are the edges a round reads through, as this process has them.
// probe says whether Claude Code's own health probe runs (`claude auth
// status` per account, side by side).
func roundDeps(probe bool) round.Deps {
	d := round.Deps{Creds: func(ctx context.Context, a accounts.Account) string {
		raw, _ := creds.ReadRaw(ctx, a.ConfigDir, a.Dir())
		return raw
	}}
	if probe {
		d.Health = round.ProbeHealth
	}
	return d
}

// markBoard sets every account's load and, under auto, the account a launch
// would take from the figures the round holds, and returns what it gathered,
// so a board that stays open can mark again as time passes. The mode is the
// round's own reading of `.current`, never a second one.
func markBoard(r *round.Round, now time.Time) *launchop.Marks {
	m := launchop.Mark(r.Set, r.Store, r.Accounts(), r.Routing.Mode, os.Environ(), now, placeDeps())
	r.Mark(m.At(now))
	return m
}

// views are the facts a renderer draws, one per account.
func views(accts []accountstate.Account) []accountstate.Facts {
	vs := make([]accountstate.Facts, len(accts))
	for i, a := range accts {
		vs[i] = a.View
	}
	return vs
}
