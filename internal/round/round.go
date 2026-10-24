// Package round is one vendor's round: the accounts discovered under a scope,
// what is known about each before any request goes out, whether each can be
// used and asked, the requests the store's claim permits, and their results
// applied to the accounts they answer. A Round owns its rows and its requests
// from Prepare until its results are applied, so no surface pairs a list with
// another round's channel, or a result with another account's row; surfaces
// read a View and drive the lifecycle.
//
// Access — whether Claude Code considers an account usable, and the request
// candidate or the attempt state that blocks one — is the second of the three
// places a vendor document is read (see DESIGN.md § A second vendor: Codex).
package round

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

// Deps are the edges a round reads through, each replaced in tests.
type Deps struct {
	// Creds reads one Claude Code account's stored credential; "" when it
	// cannot be read. ctx is the round's: a credential read is where a round
	// can stall, and the picker's cancel and the detached refresh's signals
	// stop it there.
	Creds func(ctx context.Context, a accounts.Account) string
	// Health asks Claude Code whether each account is logged in (ProbeHealth
	// in production). nil skips the probe, and health falls back to
	// credential evidence, as it does whenever the probe has no answer: the
	// detached refresh runs that way, since a process nobody is watching has
	// no use for a spawn per account.
	Health func([]accounts.Account) auth.QueryFunc
	// Start sends the requests the store's claim permits and reports each
	// result by the index of the request it answers; nil is refresh.Start.
	Start func(ctx context.Context, st *state.Store, requests []*refresh.Candidate) <-chan refresh.Result
}

// Round is one vendor's round: the scope and store it ran under, the set it
// discovered, its one read of `.current`, the problems of the vendor's own
// state file as it loaded it, and a row per account. The picker's page, the
// one-shot print, --json and limits each hold one per vendor, so mode,
// current and the marks cannot come from two reads.
type Round struct {
	Scope    config.Scope
	Store    *state.Store
	Set      accounts.Set
	Routing  accounts.Routing // one read of `.current`: mode and current together
	Problems []state.Problem  // the store's own, as this round loaded it

	rows  []*row
	start func(ctx context.Context, st *state.Store, requests []*refresh.Candidate) <-chan refresh.Result
}

// row is one account as the round holds it: its facts, and beside them the
// request a live refresh would make. Only access sets the request; a row read
// from disk alone carries none, so nothing built from it can spend.
type row struct {
	accountstate.Account
	request *refresh.Candidate
}

// View is a round as a surface shows it: the vendor, the read of `.current`,
// the store's problems and every account's facts, copied.
type View struct {
	Scope    config.Scope
	Routing  accounts.Routing
	Problems []state.Problem
	Accounts []accountstate.Account
}

// Prepare is the local half of a round: it discovers the accounts and
// assembles what is known about each one before any request goes out —
// whether Claude Code considers it usable, the newest quota figures already
// on disk, and either a request for a live refresh or the attempt state that
// blocks one.
func Prepare(ctx context.Context, scope config.Scope, st *state.Store, now time.Time, deps Deps) *Round {
	return prepare(ctx, accounts.Discover(scope), st, now, deps)
}

// prepare is Prepare over a set already discovered: access works from the
// snapshot discovery took, and never reads an account's documents again.
func prepare(ctx context.Context, set accounts.Set, st *state.Store, now time.Time, deps Deps) *Round {
	scope := set.Scope
	snap := st.Load()
	accts, routing := accountstate.Assemble(set, snap, now)
	r := &Round{Scope: scope, Store: st, Set: set, Routing: routing, Problems: snap.Problems(), rows: rowsOf(accts), start: deps.Start}
	var health auth.QueryFunc = func(string) auth.Status { return auth.Status{} }
	if scope.Vendor == config.Claude && deps.Health != nil {
		health = deps.Health(set.Accounts)
	}
	for _, d := range r.rows {
		if d.Acct.Scope.Vendor == config.Codex {
			// The Codex reader works from the auth snapshot discovery already
			// took; only Claude Code's access costs a process and a Keychain
			// read.
			codexAccess(d, now)
		} else {
			claudeAccess(d, deps.Creds(ctx, d.Acct), health(d.Acct.ConfigDir), now)
		}
	}
	return r
}

// FromDisk is a vendor's round read from disk alone, through accountstate:
// discovery, `.current`, the newest stored observation and the store's
// problems. Health is unprobed, no credential is read, and no row carries a
// request, so nothing built from it can spend one.
func FromDisk(scope config.Scope, now time.Time) *Round {
	st := state.Open(scope)
	disk := accountstate.Read(scope, st, now)
	r := Of(disk.Set, st, disk.Routing, disk.Accounts)
	r.Problems = disk.Store.Problems()
	return r
}

// Of is a round over facts a caller already holds: the accounts of set, as
// assembled under routing, against st. No account carries a request, so
// Fetch asks nobody; the facts are shown and marked like any round's.
func Of(set accounts.Set, st *state.Store, routing accounts.Routing, accts []accountstate.Account) *Round {
	return &Round{Scope: set.Scope, Store: st, Set: set, Routing: routing, rows: rowsOf(accts)}
}

func rowsOf(accts []accountstate.Account) []*row {
	rows := make([]*row, len(accts))
	for i, a := range accts {
		rows[i] = &row{Account: a}
	}
	return rows
}

// View is the round as it stands, copied for a surface to show.
func (r *Round) View() View {
	return View{Scope: r.Scope, Routing: r.Routing, Problems: r.Problems, Accounts: r.Accounts()}
}

// Accounts is every account's facts as they stand, copied.
func (r *Round) Accounts() []accountstate.Account {
	out := make([]accountstate.Account, len(r.rows))
	for i, d := range r.rows {
		out[i] = d.Account
	}
	return out
}

// Asking reports whether any account carries a request: whether a fetch
// could ask anybody at all.
func (r *Round) Asking() bool {
	return slices.ContainsFunc(r.rows, func(d *row) bool { return d.request != nil })
}

// Result is one answer of a round's Fetch: the request's result, by the index
// of the account it answers, and the round it belongs to.
type Result struct {
	refresh.Result
	of *Round
}

// Fetch is the budget half of the round: the claim, and whatever requests it
// permits, each result reported as this round's.
func (r *Round) Fetch(ctx context.Context) <-chan Result {
	requests := make([]*refresh.Candidate, len(r.rows))
	for i, d := range r.rows {
		requests[i] = d.request
	}
	start := r.start
	if start == nil {
		start = func(ctx context.Context, st *state.Store, requests []*refresh.Candidate) <-chan refresh.Result {
			return refresh.Start(ctx, st, requests, nil)
		}
	}
	return r.Feed(start(ctx, r.Store, requests))
}

// Feed reports what src delivers as this round's results, in order, and
// closes when src does: Fetch's own requests, or results a test delivers by
// hand. Its buffer holds an answer per account, so an abandoned round never
// blocks the requests still in flight.
func (r *Round) Feed(src <-chan refresh.Result) <-chan Result {
	out := make(chan Result, max(len(r.rows), cap(src)))
	go func() {
		defer close(out)
		for u := range src {
			out <- Result{Result: u, of: r}
		}
	}()
	return out
}

// Apply lands one of this round's results on the account it answers. Another
// round's result is ignored: its index counts that round's accounts, and the
// same index here can be someone else's.
func (r *Round) Apply(res Result) {
	if res.of != r || res.Index < 0 || res.Index >= len(r.rows) {
		return
	}
	d := r.rows[res.Index]
	d.View.Attempt = res.Attempt
	if res.Observation != nil {
		d.View.Obs = res.Observation
		if res.Observation.Plan != "" {
			d.View.Plan = res.Observation.Plan
		}
	}
	if res.StoreErr != nil {
		d.View.Attempt.StoreError = res.StoreErr.Error()
	}
}

// Complete fetches and applies every result: a round with nobody watching it.
func (r *Round) Complete(ctx context.Context) {
	for res := range r.Fetch(ctx) {
		r.Apply(res)
	}
}

// Mark sets each account's mark — its load and whether an automatic launch
// would take it — from marks by name; an account the marks do not name has
// none.
func (r *Round) Mark(marks map[string]accountstate.Mark) {
	for _, d := range r.rows {
		m := marks[d.Acct.Name]
		d.View.Next, d.View.Load = m.Next, m.Load
	}
}

// Narrow keeps the one account selector names. When it names none here the
// round keeps no account — its vendor's `current` is still a fact — and says
// why.
func (r *Round) Narrow(selector string) error {
	a, err := r.Set.Select(selector)
	if err != nil {
		r.rows = nil
		return err
	}
	r.rows = slices.DeleteFunc(r.rows, func(d *row) bool { return d.Acct.Name != a.Name })
	return nil
}

func claudeAccess(d *row, raw string, health auth.Status, now time.Time) {
	blob, ok := creds.Parse(raw)
	if ok {
		d.View.Plan = blob.PlanLabel()
	}
	d.View.Health = Health(health, raw, blob, ok, now.UnixMilli())
	d.View.Attempt = accountstate.Attempt{}
	if d.View.Health == accountstate.HealthOK {
		d.request, d.View.Attempt.State = refresh.Prepare(d.Acct, blob, ok, now)
	}
}

// codexAccess is the Codex health and eligibility table, first match wins.
// There is no vendor probe on this path; the auth snapshot decides. "Relogin
// required" is never produced: the document carries no refresh-token expiry,
// and expiry is read only on positive evidence.
func codexAccess(d *row, now time.Time) {
	snap := d.Acct.Auth
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
		d.request, d.View.Attempt.State = refresh.PrepareCodex(d.Acct, now)
	}
}

// Health decides one thing: can Claude Code use this account.
//
// Claude Code's own verdict settles it whenever there is one. A credential
// headroom cannot read blocks *headroom's* request, not the account — that
// belongs on the attempt axis, and letting it decide health here would
// recreate the false alarm this model exists to remove. Credential evidence
// is the fallback only when the oracle has no answer, and it never infers
// "expired" from a missing field (see creds.ReloginRequired).
func Health(st auth.Status, raw string, blob creds.Blob, blobOK bool, nowMS int64) accountstate.Health {
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

// ProbeHealth runs `claude auth status` for every account at once and hands
// back a lookup. Serially this would cost one process spawn per account
// before anything renders; in parallel it is one spawn's latency total.
func ProbeHealth(accts []accounts.Account) auth.QueryFunc {
	results := make([]auth.Status, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Go(func() {
			results[i] = auth.Query(a.ConfigDir)
		})
	}
	wg.Wait()
	byDir := make(map[string]auth.Status, len(accts))
	for i, a := range accts {
		byDir[a.ConfigDir] = results[i]
	}
	return func(configDir string) auth.Status { return byDir[configDir] }
}
