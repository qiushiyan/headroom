package launchop

// What a placement reads before the rule chooses: discovery, the stored
// observations, the session registries of this home and of every home that
// shares its ledger, the headless runs, and — for an automatic choice — the
// credentials; then which accounts may be chosen, by the caller's own policy —
// an ordinary launch's preparation and positive evidence, or a headless
// caller's eligibility. A launch, the board's mark and a headless placement
// all read through here, so they cannot hold two opinions of one account.
// Nothing here writes a file or asks the network.

import (
	"fmt"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/placement"
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

	// prepared is the launch an ordinary launch on this account would run, and
	// prepareErr why none can run here at all. A headless placement prepares
	// nothing: its caller starts the session.
	prepared   launch.Prepared
	prepareErr error
}

// facts is everything one placement read. The discovered set travels with
// what was built from it, as everywhere else.
type facts struct {
	set    accounts.Set
	snap   state.Snapshot
	home   string // this home's accounts root, as the ledger names it
	labels state.HomeLabels
	cands  []candidate

	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence

	// gone are the headless runs this reading proved over, for the next write
	// of the ledger to drop.
	gone []state.Run
}

// rule is the candidates as the rule and the store take them.
func (f facts) rule() []placement.Candidate {
	out := make([]placement.Candidate, len(f.cands))
	for i, c := range f.cands {
		out[i] = c.Candidate
	}
	return out
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

// owner resolves the session a launch names to the account that last drove
// it: "" when there is no evidence, or none that names an account still here.
func (f facts) owner(id string) string {
	name, _ := sessions.OwnerOf(id, f.refs, f.evidence[id], f.snap.Owners())
	return name
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
	accts, _ := accountstate.Assemble(set, snap, now)
	return sources(accts)
}

// gather reads what is known on disk about every account of the set, and
// which of them an ordinary launch may run on: what a launch chooses from.
func gather(set accounts.Set, st *state.Store, env []string, automatic bool, now time.Time, deps Deps) facts {
	snap := st.Load()
	return build(set, st, snap, diskSources(set, snap, now), env, automatic, now, deps)
}

// OtherHomes is the other homes spending against this home's ledger today,
// located by their registrations there. Their accounts are read for one
// thing — the sessions running on them, counted against the same
// subscriptions — and are never candidates and never owners.
func OtherHomes(scope config.Scope, snap state.Snapshot) []accounts.Home {
	if scope.Vendor != config.Claude {
		// Codex has no registry headroom can read.
		return nil
	}
	return snap.OtherHomes(scope)
}

// observe is what every placement reads of the accounts, whoever decides
// which of them may be chosen: each one's figures, the busy sessions on its
// subscription and the headless runs still working on it, and — with
// credentials — its stored login, for the refresh a choice leaves behind and
// the evidence that a login has expired. The recent launches each is counted
// against are the ledger's, read by the rule itself.
//
// Busy sessions are the subscription's, not the dir's: two dirs on one
// account, in this home or in another home sharing the ledger, spend one
// quota, so every verified-live busy session on that account counts on each
// candidate that spends it — and so does every headless run whose process
// still runs.
//
// Who may be chosen is each caller's own policy over these facts, and none is
// set here: an ordinary launch's is its preparation and positive evidence
// (build); a headless placement's is the caller's (headless.go).
func observe(set accounts.Set, st *state.Store, snap state.Snapshot, src []source, credentials bool, deps Deps) facts {
	scope := set.Scope
	f := facts{set: set, snap: snap, home: st.Home(), labels: snap.HomeLabels(), cands: make([]candidate, len(src))}

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
	load := readSessionLoad(set, keys, f.home, OtherHomes(scope, snap), snap.Runs(), deps.Probe)
	f.refs, f.evidence, f.gone = load.refs, load.evidence, load.gone

	for i, s := range src {
		c := candidate{acct: s.acct, key: s.key, obs: s.obs,
			Name: s.acct.Name, Key: s.key.ID(), Busy: load.busy[s.key.ID()], Statuses: load.statuses[s.key.ID()]}
		c.ObservedAt, c.Source, c.Limits = limitsOf(scope.Vendor, s.obs)
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
func build(set accounts.Set, st *state.Store, snap state.Snapshot, src []source, env []string, automatic bool, now time.Time, deps Deps) facts {
	f := observe(set, st, snap, src, automatic, deps)
	for i := range f.cands {
		c := &f.cands[i]
		if p, err := launch.Prepare(c.acct, set, env); err != nil {
			c.prepareErr = err
			c.Excluded, c.Unlaunchable = err.Error(), true
		} else {
			c.prepared = p
			c.Excluded = avoidReason(c.acct, c.obs, c.cred, now)
		}
	}
	return f
}

// sessionLoad is what the vendors' registries say is running right now: per
// subscription key, the sessions reported as working and every live session's
// status word, each from a claim verified by its own process — in this home
// and in every other home that shares the ledger. The refs and the evidence
// are this home's alone: they decide a session's owner, and another home's
// sessions are never this home's to route.
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

// readSessionLoad reads every account's registry, this home's and the other
// homes', with the headless runs, and inspects every claim in one pass — one
// sample per pid answers liveness, load and ownership alike. keys maps this
// home's account names to their subscription keys; another home's account
// counts only when its identity says which subscription it is, since its dir
// name is that home's to choose. Codex has no registry headroom can read: its
// accounts carry no busy sessions, and an empty read there is not evidence of
// idleness.
func readSessionLoad(set accounts.Set, keys map[string]string, home string, others []accounts.Home, runs []state.Run, probe sessions.PIDProbe) sessionLoad {
	out := sessionLoad{busy: map[string][]placement.Proc{}, statuses: map[string][]string{}}
	if set.Scope.Vendor != config.Claude {
		return out
	}
	// A claimant is whose a claim is: the subscription it spends, the home
	// that reported it, and whether it is one of this home's own accounts,
	// whose live claims alone are evidence of a session's owner.
	type claimant struct {
		key, home string
		own       bool
	}
	who := map[string]claimant{}
	var claims []sessions.RegistryEntry
	out.refs = Refs(set)
	for _, a := range set.Accounts {
		who[a.Name] = claimant{keys[a.Name], home, true}
		claims = append(claims, sessions.ReadRegistry(a.Name, a.Dir()).Entries...)
	}
	for _, o := range others {
		for _, a := range o.Set.Accounts {
			if a.AccountID == "" {
				continue
			}
			// A label no account of this home can carry: the registry reader
			// stamps it on every entry, and it is mapped back here.
			label := o.Root + "\x00" + a.Name
			who[label] = claimant{key: state.KeyFor(a).ID(), home: o.Root}
			claims = append(claims, sessions.ReadRegistry(label, a.Dir()).Entries...)
		}
	}
	for i, r := range runs {
		// Labelled like another home's accounts, mapped back below.
		label := fmt.Sprintf("\x00run\x00%d", i)
		who[label] = claimant{key: r.Key, home: r.Home}
		claims = append(claims, runEntry(label, r))
	}
	evidence, live := sessions.InspectClaims(claims, probe)
	out.gone = retiredRuns(runs, evidence)
	out.evidence = map[string]sessions.ProcessEvidence{}
	for _, e := range live {
		c := who[e.Account]
		if _, seen := out.evidence[e.SessionID]; c.own && !seen {
			// Another home's session is never this home's to route, even under
			// an id this home also holds: there it is load, and only load.
			out.evidence[e.SessionID] = sessions.ProcessEvidence{State: sessions.Live, Account: e.Account}
		}
		if c.key == "" {
			continue
		}
		out.statuses[c.key] = append(out.statuses[c.key], statusWord(e))
		if e.Busy() {
			out.busy[c.key] = append(out.busy[c.key], placement.Proc{PID: e.PID, StartedMS: e.StartedAtMS, Home: c.home})
		}
	}
	return out
}

// Refs is the set as the session collector names accounts: each one's name
// and the config dir its history and registry live in.
func Refs(set accounts.Set) []sessions.AccountRef {
	refs := make([]sessions.AccountRef, 0, len(set.Accounts))
	for _, a := range set.Accounts {
		refs = append(refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir()})
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
