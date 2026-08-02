// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"bufio"
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
	"github.com/qiushiyan/headroom/internal/throttle"
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
	case "":
		return runDashboard(cfg)
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
	case "select":
		if !noArgs() {
			return 2
		}
		return runSelect(cfg)
	case "watch":
		return runWatch(cfg, rest)
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

  (none)   usage dashboard for every account
  --json   the dashboard as JSON (schema versioned)
  select   interactively pick the account bare x targets
  watch    the dashboard on a lazy refresh loop (--interval <duration>)
  check    verify the reverse-engineered assumptions still hold
`)
}

func stdoutIsTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// accountData carries one account through the prepare → fetch → render
// pipeline.
type accountData struct {
	Acct       accounts.Account
	View       render.AccountView
	Token      string
	NeedsFetch bool
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
func prepare(cfg config.Config, th *throttle.Store) ([]*accountData, string) {
	accts := accounts.Discover(cfg)
	return prepareWith(cfg, accts, th, sources{
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
func prepareWith(cfg config.Config, accts []accounts.Account, th *throttle.Store, src sources) ([]*accountData, string) {
	current := accounts.CurrentTarget(cfg)
	now := src.now
	nowMS := now.UnixMilli()

	list := make([]*accountData, 0, len(accts))
	for _, a := range accts {
		d := &accountData{Acct: a}
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

		// Whatever Claude Code last saw is free and needs no permission from
		// the rate limiter, so it is loaded first: even a refused refresh
		// then leaves the user with numbers and their age.
		if a.Meta.CachedUsage != nil {
			if rows, err := usage.ParseLimits(a.Meta.CachedUsage); err == nil && len(rows) > 0 {
				v.Obs = &render.Observation{
					Rows:       rows,
					ObservedAt: a.Meta.FetchedAtMS / 1000,
					Source:     render.SourceCache,
				}
			}
		}

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
		case !th.Eligible(a.Name, now):
			v.Attempt.State = render.AttemptDeferred
			v.Attempt.NextEligibleAt = th.NextEligible(a.Name).Unix()
		default:
			d.Token = blob.Token
			d.NeedsFetch = true
			v.Attempt.State = render.AttemptPending
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

// launchFetches starts all usage fetches in parallel; the channel closes
// when all are done. Fetch goroutines never touch a view — the caller
// applies each result with resolve, so views have exactly one writer and a
// redraw may read every view between receives. The buffer lets senders
// finish even if the caller stops receiving early.
func launchFetches(ctx context.Context, cfg config.Config, list []*accountData, th *throttle.Store) <-chan fetchUpdate {
	client := &http.Client{Timeout: 10 * time.Second}
	updates := make(chan fetchUpdate, len(list))
	var wg sync.WaitGroup
	now := time.Now()

	// Claim every account's budget and get the claim onto disk *before* any
	// request leaves. Saving after the goroutines start leaves a window where
	// traffic is already out while another process still reads the account as
	// eligible — which is the uncoordinated double-spend this store exists to
	// prevent.
	for _, d := range list {
		if d.NeedsFetch {
			th.NoteAttempt(d.Acct.Name, now)
		}
	}
	_ = th.Save()

	for i, d := range list {
		if !d.NeedsFetch {
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
func resolve(d *accountData, res usage.Result, th *throttle.Store, now time.Time) {
	v := &d.View
	name := d.Acct.Name
	switch {
	case res.Err != nil:
		v.Attempt.State = render.AttemptTransport
	case res.StatusCode == http.StatusTooManyRequests:
		// Says nothing about the account — only that this request was too
		// soon. Rows, if any, stay exactly as they were.
		v.Attempt.State = render.AttemptRefused
		v.Attempt.HTTPCode = res.StatusCode
		th.NoteRefused(name, now)
		v.Attempt.NextEligibleAt = th.NextEligible(name).Unix()
	case res.StatusCode != http.StatusOK:
		v.Attempt.State = render.AttemptHTTP
		v.Attempt.HTTPCode = res.StatusCode
	default:
		// Any 200 proves this account's request budget recovered, whatever
		// its body turns out to say. Withholding the strike reset until rows
		// parse would make a later refusal escalate as though refusals had
		// been consecutive.
		th.NoteSuccess(name, now)

		rows, err := usage.ParseLimits(res.Body)
		if err != nil {
			v.Attempt.State = render.AttemptUnparseable
			return
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
	}
}

func runDashboard(cfg config.Config) int {
	th := throttle.Load(cfg.AccountsRoot)
	list, _ := prepare(cfg, th)
	for u := range launchFetches(context.Background(), cfg, list, th) {
		resolve(list[u.idx], u.res, th, time.Now())
	}
	_ = th.Save()
	p := render.NewPalette(stdoutIsTTY())
	now := time.Now().Unix()
	labelWidth := render.LabelWidth(views(list))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for i, d := range list {
		if i > 0 {
			fmt.Fprintln(out)
		}
		for _, line := range p.AccountBlock(d.View, now, labelWidth) {
			fmt.Fprintln(out, line)
		}
	}
	return 0
}

func views(list []*accountData) []render.AccountView {
	vs := make([]render.AccountView, len(list))
	for i, d := range list {
		vs[i] = d.View
	}
	return vs
}
