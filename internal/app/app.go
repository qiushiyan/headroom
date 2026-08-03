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
	cfg := config.Load()
	cmd := ""
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	// Every command but watch takes no further arguments; a stray one is an
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
	case "resume":
		return runResume(cfg, rest)
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
  accounts   while it is open; enter picks the account bare x targets.
             Off a terminal, prints the board once and exits.
  --json     the board as JSON (schema versioned)
  resume     interactively pick a session to resume, on the account that
             last drove it (--json lists the sessions instead)
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
func prepare(cfg config.Config, st *state.Store) ([]*accountData, string) {
	accts := accounts.Discover(cfg)
	return prepareWith(cfg, accts, st.Load(), sources{
		readRaw: creds.ReadRaw,
		health:  queryHealthParallel(accts),
		now:     time.Now(),
	})
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
	current := accounts.CurrentTarget(cfg)
	now := src.now
	nowMS := now.UnixMilli()

	list := make([]*accountData, 0, len(accts))
	for _, a := range accts {
		d := &accountData{Acct: a, Key: state.Key{UUID: a.Meta.AccountUUID, Name: a.Name}}
		list = append(list, d)

		v := &d.View
		v.Label = a.Name
		if a.Email != "" {
			v.Label = a.Email
		}
		if !a.IsPrimary() && a.Email != "" && a.Email != a.Name {
			v.DirMismatch = a.Name
		}
		v.Launcher = accounts.Launcher(a, accts, cfg.PrimaryName)
		v.Current = current == a.Name
		v.Obs = newestObservation(snap, d.Key, a.Meta, now)

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
		default:
			// What the ledger says right now, for display only. The request is
			// authorized by the claim in launchFetches and nothing else — this
			// is the last moment eligibility is a *reading* rather than a
			// decision, and treating it as permission is how two processes
			// used to both fetch.
			if next := snap.NextEligible(d.Key, now); next.After(now) {
				v.Attempt.State = render.AttemptDeferred
				v.Attempt.NextEligibleAt = next.Unix()
				continue
			}
			d.Token = blob.Token
			d.WantsFetch = true
			v.Attempt.State = render.AttemptPending
		}
	}
	return list, current
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

// fetchUpdate is one finished fetch, addressed by account index. The
// receiver applies it with resolve.
type fetchUpdate struct {
	idx int
	res usage.Result
}

// launchFetches claims the budget, then starts the permitted fetches in
// parallel; the channel closes when all are done.
//
// The claim is a test-and-set inside the store's lock and its result is what
// authorizes traffic — an account that wanted a fetch but lost the race is
// turned back into a deferred row here. Fetch goroutines never touch a view;
// the caller applies each result with resolve, so views have exactly one
// writer and a redraw may read every view between receives. The buffer lets
// senders finish even if the caller stops receiving early.
func launchFetches(ctx context.Context, cfg config.Config, list []*accountData, st *state.Store) <-chan fetchUpdate {
	client := &http.Client{Timeout: 10 * time.Second}
	updates := make(chan fetchUpdate, len(list))
	var wg sync.WaitGroup
	now := time.Now()

	idx := make([]int, 0, len(list))
	keys := make([]state.Key, 0, len(list))
	all := make([]state.Key, 0, len(list))
	for i, d := range list {
		all = append(all, d.Key)
		if d.WantsFetch {
			idx = append(idx, i)
			keys = append(keys, d.Key)
		}
	}
	// This is the one place holding a complete enumeration of the registry,
	// which is what pruning by absence requires.
	_ = st.Prune(all)
	decisions, err := st.Claim(keys, now)
	for j, dec := range decisions {
		d := list[idx[j]]
		switch {
		case dec.Permit:
			d.Permit = dec.Generation
		case err != nil:
			// The claim could not be written, so no request may go out: a
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
		go func(i int, token string) {
			defer wg.Done()
			updates <- fetchUpdate{i, usage.Fetch(ctx, client, cfg.UsageURL, token)}
		}(i, d.Token)
	}
	go func() {
		wg.Wait()
		close(updates)
	}()
	return updates
}

// resolve applies one fetch outcome. It writes the attempt axis always and
// the observation axis only on success: a refusal or a dead network must
// leave whatever was already known standing, with its own timestamp intact.
//
// The ledger is told the same story through the store, which owns every
// consequence — strike counts, cooldown arithmetic, and whether the body is
// worth keeping — and hands back when this account may next be asked, so the
// row can say so without recomputing it.
func resolve(d *accountData, res usage.Result, st *state.Store, now time.Time) {
	v := &d.View
	outcome := state.OutcomeFailed
	var body []byte

	switch {
	case res.Err != nil:
		v.Attempt.State = render.AttemptTransport
	case res.StatusCode == http.StatusTooManyRequests:
		// Says nothing about the account — only that this request was too
		// soon. Rows, if any, stay exactly as they were.
		v.Attempt.State = render.AttemptRefused
		v.Attempt.HTTPCode = res.StatusCode
		outcome = state.OutcomeRefused
	case res.StatusCode != http.StatusOK:
		v.Attempt.State = render.AttemptHTTP
		v.Attempt.HTTPCode = res.StatusCode
	default:
		rows, err := usage.ParseLimits(res.Body)
		if err != nil {
			// A 200 still proves the budget recovered, so it is spent, not
			// failed — but a body that does not parse is not worth storing.
			v.Attempt.State = render.AttemptUnparseable
			outcome = state.OutcomeSpent
			break
		}
		// Zero rows is a contractual answer — "this account reports no limit
		// windows" — and it is newer truth than any cached bars, so it
		// replaces them rather than hiding behind them.
		if len(rows) == 0 {
			v.Attempt.State = render.AttemptNoLimits
		} else {
			v.Attempt.State = render.AttemptOK
		}
		v.Obs = &render.Observation{
			Rows:       rows,
			ObservedAt: now.Unix(),
			Source:     render.SourceLive,
		}
		outcome, body = state.OutcomeStored, res.Body
	}

	next, err := st.Complete(d.Key, d.Permit, outcome, body, now)
	if err == nil && next.After(now) && v.Attempt.State != render.AttemptOK {
		v.Attempt.NextEligibleAt = next.Unix()
	}
}

func views(list []*accountData) []render.AccountView {
	vs := make([]render.AccountView, len(list))
	for i, d := range list {
		vs[i] = d.View
	}
	return vs
}
