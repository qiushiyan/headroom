// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
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
	// Every command but resume takes no further arguments; a stray one is an
	// error, not silently ignored — a misspelled flag must not fall through
	// to a command it wasn't meant for.
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
		// account set; the board itself still takes nothing.
		if cmd == "accounts" && len(rest) > 0 {
			switch rest[0] {
			case "add":
				return runAccountsAdd(cfg, rest[1:])
			case "remove":
				return runAccountsRemove(cfg, rest[1:])
			}
		}
		if !noArgs() {
			return 2
		}
		return runAccounts(cfg)
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

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: headroom [command]

  (none)     the account board: live usage for every account, refreshing
  accounts   while it is open; enter picks the account a bare launch targets.
             Off a terminal, prints the board once and exits.
  accounts add <email> [--share-config[=<dir>]]
             seed the dir for a new subscription: projects/ linked to the
             machine-global store; --share-config links the primary's
             config (settings, skills, commands, hooks, …), or every entry
             of <dir>. Then: launch --account <email> and /login once
  accounts remove <email | name.lock> [--yes]
             refuse while a session is live; delete the account's Keychain
             item and its dir; scrub .order; never touch .current
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

// accountData carries one account through the prepare → claim → fetch →
// render pipeline.
type accountData struct {
	Acct accounts.Account
	Key  state.Key
	View render.AccountView

	Token string
	// WantsFetch means the credentials are spendable and nothing local
	// objects. It is not permission: only a claim granted by the store
	// authorizes traffic, and Permit is where that lands.
	WantsFetch bool
	Permit     int64 // claim generation; 0 = no permit, no request
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
	// Strict, exactly as launch resolves it: a corrupt or dangling .current
	// marks nothing as current — the `← current` marker is a claim about where
	// bare `x` lands, and bare `x` refuses on that state. check FAILs on it;
	// enter on this board is what repairs it.
	current := ""
	if sel, err := accounts.Select(cfg, accts, ""); err == nil {
		current = sel.Name
	}
	now := src.now
	nowMS := now.UnixMilli()

	list := make([]*accountData, 0, len(accts))
	for _, a := range accts {
		d := scaffold(cfg, a, current, snap, now)
		list = append(list, d)
		v := &d.View

		raw := src.readRaw(a.ConfigDir)
		blob, blobOK := creds.Parse(raw)
		if blobOK {
			v.Plan = blob.PlanLabel()
		}
		v.Health = resolveHealth(src.health(a.ConfigDir), raw, blob, blobOK, nowMS)

		if v.Health != render.HealthOK {
			continue
		}
		switch {
		case !blobOK:
			// Blocks our request, says nothing about the account.
			v.Attempt.State = render.AttemptCredentialUnreadable
		case !blob.TokenUsable(nowMS):
			// Nothing to spend: the stored token aged out. The account is
			// fine and a session will refresh it — that is not our business.
			v.Attempt.State = render.AttemptTokenStale
		case !a.Meta.Readable:
			// Which quota bucket this dir shares is unknown, so any claim would
			// be against a bucket that might not be this account's — and the
			// budget is per account. Spending on a guess is the double-spend
			// the ledger exists to prevent, so this run asks nothing and says
			// why. Ordinarily transient: Claude Code rewrites the file
			// constantly, and the next run reads it whole.
			v.Attempt.State = render.AttemptIdentityUnknown
		default:
			// Spendable credentials and nothing local objecting. Whether a
			// request actually goes out is the claim's decision in
			// launchFetches, and *only* the claim's: reading eligibility here
			// as well, and acting on it, is how an account could be dropped
			// before the claim ever saw it — which silently emptied the key
			// list in exactly the states the store answers conservatively
			// about, so the pass that is supposed to quiet every account
			// quieted none of them.
			d.Token = blob.Token
			d.WantsFetch = true
			v.Attempt.State = render.AttemptPending
		}
	}
	return list, current
}

// scaffold assembles the network-free facts every surface knows about an
// account — identity, labels, the current marker, and the newest observation
// already on disk. prepare layers health and fetch eligibility on top; the
// limits surface stops here.
func scaffold(cfg config.Config, a accounts.Account, current string, snap state.Snapshot, now time.Time) *accountData {
	d := &accountData{Acct: a, Key: state.Key{UUID: a.Meta.AccountUUID, Name: a.Name}}
	v := &d.View
	v.Label = a.Name
	if a.Email != "" {
		v.Label = a.Email
	}
	if !a.IsPrimary() && a.Email != "" && a.Email != a.Name {
		v.DirMismatch = a.Name
	}
	v.Launcher = accounts.Launcher(cfg, a)
	v.Current = current == a.Name
	v.Obs = newestObservation(snap, d.Key, a.Meta, now)
	return d
}

// newestObservation picks the better of the two things known about an account
// before any request goes out: what headroom itself last saw, and what Claude
// Code last cached.
//
// Both are free and neither needs permission from the rate limiter, so even a
// refused refresh leaves the user with numbers and their age. Newest wins
// outright — headroom's own fetch is usually seconds old and Claude Code's
// cache hours, but an account driven in a live session while headroom sat idle
// inverts that, and the timestamp is the only honest way to choose.
//
// A body that no longer parses is skipped rather than rendered: a stored
// response that has stopped meaning anything must not present itself as "no
// limits reported". `check` is where that loss becomes a report.
func newestObservation(snap state.Snapshot, k state.Key, meta accounts.Meta, now time.Time) *render.Observation {
	var best *render.Observation
	consider := func(body []byte, atMS int64, source render.Source) {
		if len(body) == 0 || atMS <= 0 {
			return
		}
		// Zero rows is as much an answer here as it is from the live endpoint
		// — "this account reported no limit windows at time X" beats showing
		// nothing at all.
		rows, err := usage.ParseLimits(body)
		if err != nil {
			return
		}
		at := atMS / 1000
		if best != nil && at <= best.ObservedAt {
			return
		}
		best = &render.Observation{Rows: rows, ObservedAt: at, Source: source}
	}
	if obs, ok := snap.Observation(k, now); ok {
		consider(obs.Body, obs.FetchedAtMS, render.SourceStore)
	}
	consider(meta.CachedUsage, meta.FetchedAtMS, render.SourceCache)
	return best
}

// resolveHealth decides one thing: can Claude Code use this account.
//
// Claude Code's own verdict settles it whenever there is one. A credential
// headroom cannot read blocks *headroom's* request, not the account — that
// belongs on the attempt axis, and letting it decide health here would
// recreate the false alarm this rework exists to remove. Credential evidence
// is the fallback only when the oracle has no answer, and it never infers
// "expired" from a missing field (see creds.ReloginRequired).
func resolveHealth(st auth.Status, raw string, blob creds.Blob, blobOK bool, nowMS int64) render.Health {
	switch st.Outcome {
	case auth.OutcomeOK:
		if st.LoggedIn {
			return render.HealthOK
		}
		return render.HealthNoLogin
	case auth.OutcomeUnparseable:
		// The oracle ran and answered in a shape we no longer understand.
		// Guessing from credentials here would paper over vendor drift.
		return render.HealthUnknown
	case auth.OutcomeUnrunnable:
		// The probe's environment could not be built (launch refused the
		// dir). The credential fallback below reads the same broken spelling
		// and would report "no login" — a /login errand for a path bug.
		return render.HealthUnknown
	}
	switch {
	case raw == "":
		return render.HealthNoLogin
	case !blobOK:
		return render.HealthBadBlob
	case blob.ReloginRequired(nowMS):
		return render.HealthReloginRequired
	default:
		return render.HealthOK
	}
}

// fetchUpdate is one finished fetch, addressed by account index: what the
// result means, and what the ledger said when it was told. The receiver
// applies it with resolve.
//
// Both the classification and the store write happen on the fetch goroutine.
// That keeps a locked read-modify-write with an fsync off a picker's draw
// loop, and it lets the completion afford a longer wait for the lock than the
// claim does — a claim that fails costs nothing, while a completion that fails
// throws away a request already spent.
type fetchUpdate struct {
	idx   int
	out   fetchOutcome
	next  time.Time // when this account may next be asked; zero if unrecorded
	round int64     // the round idx addresses; a consumer on a later round drops it
}

// fetchOutcome is what one result means for the user, decided without touching
// a view.
type fetchOutcome struct {
	attempt render.AttemptState
	code    int
	rows    []usage.Row
	live    bool // rows are this run's own observation
}

// classify decides what a fetch result means for the user and for the ledger.
// Pure, so it can run on the goroutine that fetched: the same body always
// yields the same verdict, and nothing here writes anything.
func classify(res usage.Result) (fetchOutcome, state.Outcome, []byte) {
	switch {
	case res.Err != nil:
		return fetchOutcome{attempt: render.AttemptTransport}, state.OutcomeFailed, nil
	case res.StatusCode == http.StatusTooManyRequests:
		// Says nothing about the account — only that this request was too
		// soon. Rows, if any, stay exactly as they were.
		return fetchOutcome{attempt: render.AttemptRefused, code: res.StatusCode},
			state.OutcomeRefused, nil
	case res.StatusCode != http.StatusOK:
		return fetchOutcome{attempt: render.AttemptHTTP, code: res.StatusCode},
			state.OutcomeFailed, nil
	}
	rows, err := usage.ParseLimits(res.Body)
	if err != nil {
		// A 200 still proves the budget recovered, so it is spent, not failed
		// — but a body that does not parse is not worth storing.
		return fetchOutcome{attempt: render.AttemptUnparseable}, state.OutcomeSpent, nil
	}
	// Zero rows is a contractual answer — "this account reports no limit
	// windows" — and it is newer truth than any cached bars, so it replaces
	// them rather than hiding behind them.
	att := render.AttemptOK
	if len(rows) == 0 {
		att = render.AttemptNoLimits
	}
	return fetchOutcome{attempt: att, rows: rows, live: true}, state.OutcomeStored, res.Body
}

// launchFetches claims the budget, then starts the permitted fetches in
// parallel; the channel closes when all are done.
//
// The claim is a test-and-set inside the store's lock and its result is what
// authorizes traffic — an account that wanted a fetch but lost the race is
// turned back into a deferred row here. Fetch goroutines never touch a view;
// the caller applies each result with resolve, so views have exactly one
// writer and a redraw may read every view between receives. The buffer lets
// senders finish even if the caller stops receiving early. Every update
// carries the caller's round stamp: idx addresses positions in the list this
// round was built over, and stamping makes "never applied to a rebuilt list"
// a property of the data rather than of caller discipline.
func launchFetches(ctx context.Context, cfg config.Config, list []*accountData, st *state.Store, round int64) <-chan fetchUpdate {
	client := &http.Client{Timeout: 10 * time.Second}
	updates := make(chan fetchUpdate, len(list))
	var wg sync.WaitGroup
	now := time.Now()

	idx := make([]int, 0, len(list))
	keys := make([]state.Key, 0, len(list))
	for i, d := range list {
		if d.WantsFetch {
			idx = append(idx, i)
			keys = append(keys, d.Key)
		}
	}
	decisions, err := st.Claim(keys, now)
	for j, dec := range decisions {
		d := list[idx[j]]
		switch {
		case dec.Permit:
			d.Permit = dec.Generation
		case err != nil || dec.Degraded:
			// The claim could not be written or could not be reasoned from, so
			// no request may go out — and the row says whose problem that is. A
			// claim that never reached disk is one another process cannot see.
			d.WantsFetch = false
			d.View.Attempt.State = render.AttemptStateUnavailable
		default:
			d.WantsFetch = false
			d.View.Attempt.State = render.AttemptDeferred
			d.View.Attempt.NextEligibleAt = dec.NextEligible.Unix()
		}
	}

	for i, d := range list {
		if !d.WantsFetch || d.Permit == 0 {
			continue
		}
		wg.Add(1)
		// Everything the goroutine needs is copied in: it never reads or
		// writes an accountData, so views stay single-writer by construction
		// rather than by discipline.
		go func(i int, k state.Key, permit int64, token string) {
			defer wg.Done()
			out, outcome, body := classify(usage.Fetch(ctx, client, cfg.UsageURL, token))
			var next time.Time
			if ctx.Err() == nil {
				// A cancelled fetch says nothing about the budget and its claim
				// already stands. Completing it on the way out would only risk
				// a half-written temp file as the process exits.
				next, _ = st.Complete(k, permit, outcome, body, time.Now())
			}
			updates <- fetchUpdate{i, out, next, round}
		}(i, d.Key, d.Permit, d.Token)
	}
	go func() {
		wg.Wait()
		close(updates)
	}()
	return updates
}

// resolve applies one finished fetch to its account's view, and touches
// nothing else. It writes the attempt axis always and the observation axis
// only on success: a refusal or a dead network must leave whatever was already
// known standing, with its own timestamp intact.
//
// The ledger was already told the same story by the fetch goroutine, through
// the store, which owns every consequence — strike counts, cooldown arithmetic
// and whether the body is worth keeping — and handed back when this account may
// next be asked, so the row can say so without recomputing it.
func resolve(d *accountData, u fetchUpdate, now time.Time) {
	v := &d.View
	v.Attempt.State = u.out.attempt
	v.Attempt.HTTPCode = u.out.code
	if u.out.live {
		v.Obs = &render.Observation{
			Rows:       u.out.rows,
			ObservedAt: now.Unix(),
			Source:     render.SourceLive,
		}
	}
	if u.next.After(now) && u.out.attempt != render.AttemptOK {
		v.Attempt.NextEligibleAt = u.next.Unix()
	}
}

func views(list []*accountData) []render.AccountView {
	vs := make([]render.AccountView, len(list))
	for i, d := range list {
		vs[i] = d.View
	}
	return vs
}
