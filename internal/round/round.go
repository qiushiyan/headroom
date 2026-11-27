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
	"os"
	"slices"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/peers"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

// Deps are the edges a round reads through, each replaced in tests.
type Deps struct {
	// Creds reads one Claude Code account's stored credential, and where it
	// came from; an empty reading when it cannot be read. ctx is the round's:
	// a credential read is where a round can stall, and the picker's cancel
	// and the detached refresh's signals stop it there.
	Creds func(ctx context.Context, a accounts.Account) creds.Reading
	// Item says what the Keychain shows of one account's item, without its
	// secret (creds.KeychainItem in production). It is asked only for an
	// account whose read did not come from the Keychain and whose health
	// would otherwise be a bad answer, since only there does it change what
	// is said; nil asks nothing, and such a read stands as it is.
	Item func(ctx context.Context, a accounts.Account) (creds.Item, string)
	// Health asks Claude Code whether each account is logged in (ProbeHealth
	// in production). nil skips the probe, and health falls back to
	// credential evidence, as it does whenever the probe has no answer: the
	// detached refresh runs that way, since a process nobody is watching has
	// no use for a spawn per account.
	Health func([]accounts.Account) auth.QueryFunc
	// Start sends the requests the store's claim permits and reports each
	// result by the index of the request it answers; nil is refresh.Start.
	Start func(ctx context.Context, st *state.Store, requests []*refresh.Candidate) <-chan refresh.Result
	// Pull asks another machine for its readings (peers.SSH in production).
	// nil pulls from nobody, and so does a scope that names no peers.
	Pull peers.Asker
}

// Round is one vendor's round: the scope and store it ran under, the set it
// discovered, the problems of the vendor's own state file as it loaded it,
// and a row per account. The picker's page, the one-shot print, --json and
// limits each hold one per vendor, so the rows and the marks cannot come from
// two reads.
type Round struct {
	Scope    config.Scope
	Store    *state.Store
	Set      accounts.Set
	Problems []state.Problem // the store's own, as this round loaded it

	rows  []*row
	start func(ctx context.Context, st *state.Store, requests []*refresh.Candidate) <-chan refresh.Result
	pull  peers.Asker
}

// row is one account as the round holds it: its facts, and beside them the
// request a live refresh would make. Only access sets the request; a row read
// from disk alone carries none, so nothing built from it can spend.
type row struct {
	accountstate.Account
	request *refresh.Candidate
}

// View is a round as a surface shows it: the vendor, the store's problems and
// every account's facts, copied.
type View struct {
	Scope    config.Scope
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
	accts := accountstate.Assemble(set, snap, now)
	r := &Round{Scope: scope, Store: st, Set: set, Problems: snap.Problems(), rows: rowsOf(accts), start: deps.Start, pull: deps.Pull}
	var health auth.QueryFunc = func(string) auth.Status { return auth.Status{} }
	if scope.Vendor == config.Claude && deps.Health != nil {
		health = deps.Health(set.Accounts)
	}
	for _, d := range r.rows {
		switch d.Acct.Scope.Vendor {
		case config.Codex:
			// The Codex reader works from the auth snapshot discovery already
			// took; only Claude Code's access costs a process and a Keychain
			// read.
			codexAccess(d, now)
		case config.Agy:
			agyAccess(d, os.Environ())
		case config.Claude:
			claudeAccess(ctx, d, deps.Creds(ctx, d.Acct), health(d.Acct.ConfigDir), deps.Item, now)
		}
	}
	return r
}

// FromDisk is a vendor's round read from disk alone, through accountstate:
// discovery, the newest stored observation and the store's problems. Health is unprobed, no credential is read, and no row carries a
// request, so nothing built from it can spend one.
func FromDisk(scope config.Scope, now time.Time) *Round {
	st := state.Open(scope)
	disk := accountstate.Read(scope, st, now)
	r := Of(disk.Set, st, disk.Accounts)
	r.Problems = disk.Store.Problems()
	return r
}

// Of is a round over facts a caller already holds: the accounts of set,
// against st. No account carries a request, so Fetch asks nobody; the facts
// are shown and marked like any round's.
func Of(set accounts.Set, st *state.Store, accts []accountstate.Account) *Round {
	return &Round{Scope: set.Scope, Store: st, Set: set, rows: rowsOf(accts)}
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
	return View{Scope: r.Scope, Problems: r.Problems, Accounts: r.Accounts()}
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
// of the account it answers, and the round it belongs to. In a round that
// pulls, selection carries the figures the accounts it covers come to, chosen
// from the store after the answer landed; a pull's result is no request's
// (pulled), and carries every account's selection, or problem — this machine
// failing to claim or record the pull.
type Result struct {
	refresh.Result
	of        *Round
	selection map[int]*accountstate.Observation
	pulled    bool
	problem   string
}

// event is what reaches a pulling round's selector: a request's result, a
// pull that landed, or this machine failing to record one.
type event struct {
	res     *refresh.Result
	pulled  bool
	problem string
}

// Fetch is the budget half of the round: the claim, and whatever requests it
// permits, each result reported as this round's — and beside them, a pull
// from each machine the scope names whose pull is due, its readings reported
// as results of the accounts they are of.
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
	if r.pull == nil || len(r.Scope.Peers) == 0 {
		return r.Feed(start(ctx, r.Store, requests))
	}
	// Figures then arrive from two places at once — this machine's requests
	// and the other machines' answers — and the board must come to what the
	// store holds, whatever order they land in. So every arrival is turned
	// into figures in one place, in order: the selector reads the store after
	// each and chooses the affected accounts' figures by the assembly's rule
	// (accountstate.Select), with any answer of this process the store could
	// not record as one more candidate. Each choice is made from a store at
	// least as new as the one before it, so the last to land is the newest;
	// Apply only shows what it is handed.
	//
	// The pulls start first: the claim inside start can wait on the lock,
	// and a pull has no reason to wait on it.
	accts := r.Accounts()
	events := make(chan event, len(r.rows)+len(r.Scope.Peers))
	var wg sync.WaitGroup
	for _, peer := range r.Scope.Peers {
		wg.Go(func() {
			answered, err := peers.Pull(ctx, r.Store, peer, r.pull)
			switch {
			case err != nil:
				events <- event{problem: err.Error()}
			case answered:
				events <- event{pulled: true}
			}
		})
	}
	src := start(ctx, r.Store, requests)
	wg.Go(func() {
		for u := range src {
			events <- event{res: &u}
		}
	})
	go func() {
		wg.Wait()
		close(events)
	}()
	out := make(chan Result, len(r.rows)+len(r.Scope.Peers))
	go func() {
		defer close(out)
		unrecorded := map[int]*accountstate.Observation{}
		choose := func(snap state.Snapshot, now time.Time, i int) *accountstate.Observation {
			a := accts[i]
			return accountstate.Newest(unrecorded[i], accountstate.Select(snap, a.Key, a.Acct, now))
		}
		for ev := range events {
			switch {
			case ev.problem != "":
				out <- Result{of: r, problem: ev.problem}
			case ev.pulled:
				snap, now := r.Store.Load(), time.Now()
				sel := make(map[int]*accountstate.Observation, len(accts))
				for i := range accts {
					sel[i] = choose(snap, now, i)
				}
				out <- Result{of: r, selection: sel, pulled: true}
			default:
				u := *ev.res
				res := Result{Result: u, of: r}
				if u.Index >= 0 && u.Index < len(accts) {
					if u.Observation != nil && u.StoreErr != nil {
						unrecorded[u.Index] = u.Observation
					}
					res.selection = map[int]*accountstate.Observation{u.Index: choose(r.Store.Load(), time.Now(), u.Index)}
				}
				out <- res
			}
		}
	}()
	return out
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
	if res.of != r {
		return
	}
	if res.problem != "" {
		r.Problems = append(slices.Clip(r.Problems), state.Problem{Section: "peers", Detail: res.problem})
		return
	}
	for i, o := range res.selection {
		if i >= 0 && i < len(r.rows) {
			r.rows[i].View.Show(o)
		}
	}
	if res.pulled || res.Index < 0 || res.Index >= len(r.rows) {
		return
	}
	d := r.rows[res.Index]
	d.View.Attempt = res.Attempt
	if res.selection == nil && res.Observation != nil {
		// A round that pulls from nobody shows each answer as it lands, as
		// the store records it.
		d.View.Show(res.Observation)
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
		d.View.Next, d.View.Load, d.View.Reserved = m.Next, m.Load, m.Reserved
	}
}

// Narrow keeps the one account selector names. When it names none here the
// round keeps no account, and says why.
func (r *Round) Narrow(selector string) error {
	a, err := r.Set.Select(selector)
	if err != nil {
		r.rows = nil
		return err
	}
	r.rows = slices.DeleteFunc(r.rows, func(d *row) bool { return d.Acct.Name != a.Name })
	return nil
}

func claudeAccess(ctx context.Context, d *row, read creds.Reading, health auth.Status, item func(context.Context, accounts.Account) (creds.Item, string), now time.Time) {
	blob, ok := creds.Parse(read.Raw)
	if ok {
		d.View.SetLoginPlan(blob.PlanLabel())
	}
	d.View.Health = Health(health, read.Raw, blob, ok, now.UnixMilli())
	d.View.Login = accountstate.LoginReached
	if reach := loginReach(ctx, d.Acct, read, d.View.Health, item); reach != accountstate.LoginReached {
		// A bad answer reached without the login sessions use is no answer:
		// the account is as it is wherever that login can be read.
		d.View.Health, d.View.Login = accountstate.HealthUnknown, reach
	}
	d.View.Attempt = accountstate.Attempt{}
	if d.View.Health == accountstate.HealthOK {
		d.request, d.View.Attempt.State = refresh.Prepare(d.Acct, blob, ok, now)
	}
}

// loginReach asks where the account's login is kept when the answer would
// change what is said: the read did not come from the Keychain, and health
// came out as something a person would be sent to fix — no login, an expired
// one, an unreadable credential. A Keychain item that exists and was not
// handed over, or that `security` cannot answer for, means that verdict was
// reached on a file or on nothing, while the login sessions use sits unread:
// `claude auth status` itself reports such an account logged out (DESIGN.md §
// Renewing logins). The rule is creds.Reading.Access, which `login` holds an
// account back by and `check` words its credential line with.
func loginReach(ctx context.Context, a accounts.Account, read creds.Reading, health accountstate.Health, item func(context.Context, accounts.Account) (creds.Item, string)) accountstate.LoginReach {
	switch health {
	case accountstate.HealthNoLogin, accountstate.HealthReloginRequired, accountstate.HealthBadBlob:
	default:
		return accountstate.LoginReached
	}
	if read.Source == creds.SourceKeychain || ctx.Err() != nil {
		return accountstate.LoginReached
	}
	var found creds.Item = creds.ItemAbsent
	var said string
	if item != nil {
		found, said = item(ctx, a)
	}
	if ctx.Err() != nil {
		// A question cut short is not an answer about the Keychain.
		return accountstate.LoginReached
	}
	switch access, _ := read.Access(found, said); access {
	case creds.Withheld:
		return accountstate.LoginWithheld
	case creds.Unplaced:
		return accountstate.LoginUnplaced
	}
	return accountstate.LoginReached
}

// agyAccess is Antigravity's: there is no question to ask agy about its
// sign-in short of the read itself, and headroom reads nothing of the login.
// An installed agy — the scope is present, or there would be no round — is
// taken as usable, and a read that fails says so on the attempt axis, never
// as a login problem it has no evidence for.
func agyAccess(d *row, environ []string) {
	d.View.Health = accountstate.HealthOK
	d.View.Attempt = accountstate.Attempt{}
	d.request, d.View.Attempt.State = refresh.PrepareAgy(d.Acct, environ)
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
