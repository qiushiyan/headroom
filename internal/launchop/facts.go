package launchop

// What a placement reads before the rule chooses: discovery, the stored
// observations, this home's session registries, the headless runs recorded
// against its accounts root, and — for an automatic choice — the credentials; then which accounts may be chosen, by the caller's own policy —
// an ordinary launch's preparation and positive evidence, or a headless
// caller's eligibility. A launch, the board's mark and a headless placement
// all read through here, so they cannot hold two opinions of one account.
// Nothing here writes a file or asks the network.

import (
	"cmp"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

// candidate is one account as a placement read it: what the rule is handed,
// and beside it what the operation around the rule needs of the same account.
// It is one value, built once per account, so a candidate's figures can never
// be read under another's key or launched with another's preparation.
type candidate struct {
	placement.Candidate

	acct accounts.Account
	key  state.Key                 // the ledger key its figures and claims are filed under
	obs  *accountstate.Observation // the observation its figures came from, as handed in

	// cred is the account's stored credential, read only when the rule
	// chooses: "" when it was not read, and when it could not be.
	cred string

	// standing is how a reservation stands to this launch — reservedHere,
	// reservedElsewhere, or "" for none — and reserved what it is for, in a
	// person's words. Neither reaches the rule: elsewhere is an exclusion.
	standing, reserved string

	// prepared is the launch an ordinary launch on this account would run, and
	// prepareErr why none can run here at all. A headless placement prepares
	// nothing: its caller starts the session.
	prepared   launch.Prepared
	prepareErr error
}

// facts is everything one placement read. The discovered set travels with
// what was built from it, as everywhere else.
type facts struct {
	set   accounts.Set
	snap  state.Snapshot
	cands []candidate

	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence

	// gone are the headless runs this reading proved over, for the next write
	// of the ledger to drop.
	gone []state.Run

	// reservedErr is why `.reserved` could not be read. Nothing then says which
	// accounts are kept for what, so no automatic choice can be made from
	// these facts; a launch told where to run does not need to know.
	reservedErr error
}

// keptOut is why a reservation keeps this account from this launch, or "".
func (c candidate) keptOut() string {
	if c.standing == reservedElsewhere {
		return c.reserved
	}
	return ""
}

// reservations is `.reserved` as one placement read it: which accounts are
// kept for what, or why nothing can say.
type reservations struct {
	held accounts.Reservations
	err  error
}

func readReservations(scope config.Scope) reservations {
	held, err := accounts.ReadReservations(scope)
	return reservations{held, err}
}

// keeps says a reservation keeps the account from work in project, or that
// nothing can say it does not: an unreadable `.reserved` never releases an
// account.
func (r reservations) keeps(ref, project string) bool {
	s, _ := standing(r.held, ref, project)
	return r.err != nil || s == reservedElsewhere
}

// How a reservation stands to one launch, as the launch log spells it.
const (
	reservedHere      = "here"      // kept for the repository this launch runs in: ranked like any other
	reservedElsewhere = "elsewhere" // kept for others: keptOut says which
)

// standing is how a reservation stands to a launch in project, and what it
// is for in a person's words. An account whose repositories are all gone is
// still kept from everything else: a missing directory is not permission.
func standing(held accounts.Reservations, ref, project string) (string, string) {
	if !held.Held(ref) {
		return "", ""
	}
	dirs := make([]string, 0, len(held[ref]))
	for _, d := range held[ref] {
		dirs = append(dirs, render.Tilde(d))
	}
	words := "reserved for " + strings.Join(dirs, ", ")
	if held.Admits(ref, project) {
		return reservedHere, words
	}
	return reservedElsewhere, words
}

// rule is the candidates as the rule and the store take them.
func (f facts) rule() []placement.Candidate {
	out := make([]placement.Candidate, len(f.cands))
	for i, c := range f.cands {
		out[i] = c.Candidate
	}
	return out
}

// standing is how the named account's reservation stood to the launch.
func (f facts) standing(name string) string {
	c, _ := f.find(name)
	return c.standing
}

// find is the candidate of that name.
func (f facts) find(name string) (candidate, bool) {
	for _, c := range f.cands {
		if c.Name == name {
			return c, true
		}
	}
	return candidate{}, false
}

// source is one account as a surface holds it: who it is, the quota it
// spends, and the newest observation that surface has of it.
type source struct {
	acct accounts.Account
	key  state.Key
	obs  *accountstate.Observation
}

// sources is the facts a surface holds about each account, as a placement
// reads them.
func sources(accts []accountstate.Account) []source {
	src := make([]source, len(accts))
	for i, a := range accts {
		src[i] = source{acct: a.Acct, key: a.Key, obs: a.View.Obs}
	}
	return src
}

// diskSources is every account of the set with its newest usable stored
// observation: the disk's view, which a launch and a headless placement read.
func diskSources(set accounts.Set, snap state.Snapshot, now time.Time) []source {
	return sources(accountstate.Assemble(set, snap, now))
}

// gather reads what is known on disk about every account of the set, and
// which of them an ordinary launch in dir may run on: what a launch chooses
// from.
func gather(set accounts.Set, st *state.Store, res reservations, env []string, dir string, automatic bool, now time.Time, deps Deps) facts {
	snap := st.Load()
	return build(set, st, snap, diskSources(set, snap, now), res, env, dir, automatic, now, deps)
}

// observe is what every placement reads of the accounts, whoever decides
// which of them may be chosen: each one's figures, the busy sessions on its
// subscription and the headless runs still working on it, and — with
// credentials — its stored login, for the refresh a choice leaves behind and
// the evidence that a login has expired. The recent launches each is counted
// against are the ledger's, read by the rule itself.
//
// Busy sessions are the subscription's, not the dir's: two dirs on one
// account spend one quota, so every verified-live busy session on that
// account counts on each candidate that spends it — and so does every
// headless run whose process still runs.
//
// Who may be chosen is each caller's own policy over these facts, and none is
// set here: an ordinary launch's is its preparation and positive evidence
// (build); a headless placement's is the caller's (headless.go). What holds
// for all of them is read here: how each account's reservation stands to a
// launch in dir, which each policy composes as keptOut.
func observe(set accounts.Set, st *state.Store, snap state.Snapshot, src []source, res reservations, dir string, credentials bool, deps Deps) facts {
	scope := set.Scope
	f := facts{set: set, snap: snap, cands: make([]candidate, len(src)), reservedErr: res.err}
	project := sessions.RepoKey(dir)

	var blobs []string
	if credentials && scope.Vendor == config.Claude {
		accts := make([]accounts.Account, len(src))
		for i, s := range src {
			accts[i] = s.acct
		}
		blobs = readCredsParallel(accts, deps.Creds)
	}
	keys := make(map[string]string, len(src))
	for _, s := range src {
		keys[s.acct.Name] = s.key.ID()
	}
	load := readSessionLoad(set, keys, snap.Runs(), deps.Probe)
	f.refs, f.evidence, f.gone = load.refs, load.evidence, load.gone

	for i, s := range src {
		c := candidate{acct: s.acct, key: s.key, obs: s.obs,
			Name: s.acct.Name, Ref: s.acct.Ref(), Key: s.key.ID(), Busy: load.busy[s.key.ID()], Statuses: load.statuses[s.key.ID()]}
		c.ObservedAt, c.Source, c.Limits = limitsOf(scope.Vendor, s.obs)
		c.standing, c.reserved = standing(res.held, s.acct.Ref(), project)
		if blobs != nil {
			c.cred = blobs[i]
		}
		f.cands[i] = c
	}
	return f
}

// build is the one place an account becomes a candidate for an ordinary
// launch, for a launch and for the board's mark alike: the two differ only in
// which observations they hand in — the disk's, or the ones on screen.
// Everything that decides whether an account may be chosen is read here, the
// same way, so the mark and the launch cannot hold two opinions of one
// account.
//
// automatic says the rule will choose. Only then are credentials read: they
// are the evidence that a login has expired, which matters to a choice made
// for the person and not to a launch on an account they named — launching a
// logged-out account by name is how one logs in.
//
// An account that cannot be launched at all — its preparation fails — is
// excluded from every choice, a forced one included. Otherwise it is excluded
// from an automatic choice only on positive evidence: its identity document
// parsed and names nobody, its refresh token is demonstrably expired, or the
// vendor says it is blocked. A credential that could not be read excludes
// nothing. With a locked Keychain every account reads that way, and in auto
// mode a usable account must never be refused for want of bookkeeping. The
// vendor's health probe is not consulted: it can say "logged in" over a
// refresh token that has expired, and a launch has only the credential to go
// by.
func build(set accounts.Set, st *state.Store, snap state.Snapshot, src []source, res reservations, env []string, dir string, automatic bool, now time.Time, deps Deps) facts {
	f := observe(set, st, snap, src, res, dir, automatic, deps)
	for i := range f.cands {
		c := &f.cands[i]
		if p, err := launch.Prepare(c.acct, set, env); err != nil {
			c.prepareErr = err
			c.Excluded, c.Unlaunchable = err.Error(), true
		} else {
			c.prepared = p
			c.Excluded = cmp.Or(avoidReason(c.acct, c.obs, c.cred, now), c.keptOut())
		}
	}
	return f
}

// sessionLoad is what the vendors' registries say is running right now: per
// subscription key, the sessions reported as working and every live session's
// status word, each from a claim verified by its own process. The refs and the
// evidence decide a session's owner.
//
// Headless runs join them: a process a headless caller started and reported
// is working for as long as it runs, so a verified-live run is a busy session
// of the subscription it was placed on. Its config dir is the caller's and is
// read by nobody; the run's record is the claim, verified the same way. gone
// are the runs proven over.
type sessionLoad struct {
	busy     map[string][]placement.Proc
	statuses map[string][]string
	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence
	gone     []state.Run
}

// readSessionLoad reads every account's registry, with the headless runs, and
// inspects every claim in one pass — one sample per pid answers liveness,
// load and ownership alike. keys maps account names to their subscription
// keys. Codex has no registry headroom can read: its accounts carry no busy
// sessions, and an empty read there is not evidence of idleness.
func readSessionLoad(set accounts.Set, keys map[string]string, runs []state.Run, probe sessions.PIDProbe) sessionLoad {
	out := sessionLoad{busy: map[string][]placement.Proc{}, statuses: map[string][]string{}}
	if set.Scope.Vendor != config.Claude {
		return out
	}
	// A claimant is whose a claim is: the subscription it spends, and whether
	// it is an account's own, whose live claims alone are evidence of a
	// session's owner — a headless run is load, and only load.
	type claimant struct {
		key string
		own bool
	}
	who := map[string]claimant{}
	var claims []sessions.RegistryEntry
	out.refs = Refs(set)
	for _, a := range set.Accounts {
		who[a.Name] = claimant{keys[a.Name], true}
		claims = append(claims, sessions.ReadRegistry(a.Name, a.Dir()).Entries...)
	}
	for i, r := range runs {
		// A label no account can carry, mapped back below.
		label := fmt.Sprintf("\x00run\x00%d", i)
		who[label] = claimant{key: r.Key}
		claims = append(claims, runEntry(label, r))
	}
	evidence, live := sessions.InspectClaims(claims, probe)
	out.gone = retiredRuns(runs, evidence)
	out.evidence = map[string]sessions.ProcessEvidence{}
	for _, e := range live {
		c := who[e.Account]
		if _, seen := out.evidence[e.SessionID]; c.own && !seen {
			out.evidence[e.SessionID] = sessions.ProcessEvidence{State: sessions.Live, Account: e.Account}
		}
		if c.key == "" {
			continue
		}
		out.statuses[c.key] = append(out.statuses[c.key], statusWord(e))
		if e.Busy() {
			out.busy[c.key] = append(out.busy[c.key], placement.Proc{PID: e.PID, StartedMS: e.StartedAtMS})
		}
	}
	return out
}

// Refs is the set as the session collector names accounts: each one's name
// and the config dir its history and registry live in.
func Refs(set accounts.Set) []sessions.AccountRef {
	refs := make([]sessions.AccountRef, 0, len(set.Accounts))
	for _, a := range set.Accounts {
		refs = append(refs, sessions.AccountRef{Name: a.Name, Ref: a.Ref(), Dir: a.Dir()})
	}
	return refs
}

// runEntry is a headless run as a registry claim: working, for as long as
// its process runs.
func runEntry(label string, r state.Run) sessions.RegistryEntry {
	return sessions.RegistryEntry{
		Account: label, SessionID: runID(r), PID: r.PID, StartedAtMS: r.StartedMS,
		OK: true, Status: sessions.StatusBusy, StatusState: tag.OK,
	}
}

// runID is the session id a run's claim carries: one no vendor session has.
func runID(r state.Run) string { return "run:" + r.Placement }

// retiredRuns is the runs the inspected claims prove over, for the next write
// of the ledger to drop. A run whose process could not be inspected is kept:
// failing to inspect is never evidence of absence.
func retiredRuns(runs []state.Run, evidence map[string]sessions.ProcessEvidence) []state.Run {
	var gone []state.Run
	for _, r := range runs {
		if evidence[runID(r)].State == sessions.NotLive {
			gone = append(gone, r)
		}
	}
	return gone
}

// limitsOf turns an observation into the rows the rule counts: the ones that
// bound ordinary work on the account, with the session window marked. Which
// rows those are is the usage package's to say.
func limitsOf(vendor config.Vendor, obs *accountstate.Observation) (observedAt int64, src string, limits []placement.Limit) {
	if obs == nil {
		return 0, "", nil
	}
	session := usage.SessionWindow(vendor, obs.Rows)
	for j, r := range obs.Rows {
		if !usage.General(vendor, r) {
			continue
		}
		limits = append(limits, placement.Limit{
			Kind: r.Kind, Label: r.Label, Percent: r.Percent,
			Bad: r.PercentState == tag.Bad, ResetAt: r.ResetAt, Window: r.WindowSeconds,
			Period: int64(usage.Period(vendor, r) / time.Second), Session: j == session,
		})
	}
	return obs.ObservedAt, obs.Source.Name(), limits
}

// statusWord is a live session's status as the vendor wrote it. Absent and
// unreadable are said as such: neither is "idle".
func statusWord(e sessions.RegistryEntry) string {
	switch {
	case e.Status != "":
		return e.Status
	case e.StatusState == tag.Bad:
		return "(unreadable)"
	default:
		return "(none)"
	}
}

// avoidReason is why an automatic choice must not land on this account, or "".
// Positive evidence only — see build.
func avoidReason(a accounts.Account, obs *accountstate.Observation, raw string, now time.Time) string {
	switch {
	case a.Scope.Vendor == config.Codex && a.Auth.State == codexauth.Absent:
		return "not logged in"
	case a.Scope.Vendor == config.Codex && (a.Auth.State == codexauth.Unreadable || a.Auth.State == codexauth.NoTokens):
		return "login unreadable"
	case a.Scope.Vendor == config.Claude && a.Readable && a.Email == "":
		return "not logged in"
	}
	if blob, ok := creds.Parse(raw); ok && blob.ReloginRequired(now.UnixMilli()) {
		return "login expired"
	}
	if obs != nil && obs.Allowance.State == usage.AllowanceBlocked {
		return "blocked by the vendor"
	}
	return ""
}

// readCredsParallel reads every account's credential at once: serially it
// would cost one `security` spawn per account before the vendor starts.
func readCredsParallel(accts []accounts.Account, read func(accounts.Account) string) []string {
	out := make([]string, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Go(func() {
			out[i] = read(a)
		})
	}
	wg.Wait()
	return out
}
