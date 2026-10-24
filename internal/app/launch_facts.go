package app

// What a placement reads before the rule chooses: discovery, each account's
// preparation, the stored observations, the session registries of this home
// and of every home that shares its ledger, the headless runs, and — for an
// automatic choice — the credentials. The launch, the board's mark and a
// headless placement all read through here, so they cannot hold two opinions
// of one account. Nothing here writes a file or asks the network: the launch
// operation records the choice (launch_operation.go), and figures are
// refreshed for the *next* launch by a detached round (refresh.go), so the
// command typed most never waits on an endpoint.

import (
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

// The edges a placement reads through, injected so the launch path can be
// tested without a Keychain or a process table.
var (
	// placementCreds reads one Claude Code account's credential blob.
	placementCreds = func(a accounts.Account) string {
		raw, _ := creds.ReadRaw(a.ConfigDir, a.Dir())
		return raw
	}
	// placementProbe answers when a pid started, for registry liveness.
	placementProbe sessions.PIDProbe = psProbe
)

// placeFacts is everything one placement read. The discovered set travels with
// what was built from it, as everywhere else.
type placeFacts struct {
	set   accounts.Set
	snap  state.Snapshot
	home  string // this home's accounts root, as the ledger names it
	cands []placement.Candidate
	keys  []state.Key // the ledger key of each candidate, in order

	// askable marks the candidates whose stored token could carry a usage
	// request right now. Known only when credentials were read.
	askable []bool

	// prepared holds the launch each launchable account would run; prepareErr
	// holds why the others cannot be launched at all.
	prepared   map[string]launch.Prepared
	prepareErr map[string]error

	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence

	// obs is each candidate's observation, in order, as it was handed in.
	obs []*accountstate.Observation

	// gone are the headless runs this reading proved over, for the next write
	// of the ledger to drop.
	gone []state.Run
}

func (f placeFacts) account(name string) (accounts.Account, bool) {
	for _, a := range f.set.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// owner resolves the session a launch names to the account that last drove
// it: "" when there is no evidence, or none that names an account still here.
func (f placeFacts) owner(id string) string {
	name, _ := sessions.OwnerOf(id, f.refs, f.evidence[id], f.snap.Owners())
	return name
}

// placeSource is one account as a surface holds it: who it is, the quota it
// spends, and the newest observation that surface has of it.
type placeSource struct {
	acct accounts.Account
	key  state.Key
	obs  *accountstate.Observation
}

// gatherPlacement reads what is known on disk about every account of the set:
// what a launch chooses from.
func gatherPlacement(set accounts.Set, st *state.Store, env []string, automatic bool, now time.Time) placeFacts {
	snap := st.Load()
	facts, _ := accountstate.Assemble(set, snap, now)
	src := make([]placeSource, len(facts))
	for i, fa := range facts {
		src[i] = placeSource{acct: fa.Acct, key: fa.Key, obs: fa.View.Obs}
	}
	f := buildCandidates(set, src, env, automatic, now, st.Home(), otherHomes(set.Scope, snap), snap.Runs())
	f.snap = snap
	return f
}

// otherHomes is the other homes spending against this home's ledger today,
// located by their registrations there. Their accounts are read for one
// thing — the sessions running on them, counted against the same
// subscriptions — and are never candidates and never owners.
func otherHomes(scope config.Scope, snap state.Snapshot) []accounts.Home {
	if scope.Vendor != config.Claude {
		// Codex has no registry headroom can read.
		return nil
	}
	return snap.OtherHomes(scope)
}

// homeLabels names every home a surface may mention, by accounts root: the
// home dir's last element ("steward-home" for ~/.steward-home), which is what
// a person calls it.
func homeLabels(snap state.Snapshot, self string) map[string]string {
	out := map[string]string{}
	for _, m := range snap.Members() {
		out[m.Root] = strings.TrimPrefix(filepath.Base(m.Home), ".")
	}
	if _, ok := out[self]; !ok {
		out[self] = strings.TrimPrefix(filepath.Base(filepath.Dir(self)), ".")
	}
	return out
}

func homeLabel(labels map[string]string, root string) string {
	if l, ok := labels[root]; ok && l != "" {
		return l
	}
	return strings.TrimPrefix(filepath.Base(filepath.Dir(root)), ".")
}

// buildCandidates is the one place an account becomes a placement candidate,
// for a launch and for the board's mark alike: the two differ only in which
// observations they hand in — the disk's, or the ones on screen. Everything
// that decides whether an account may be chosen is read here, the same way,
// so the mark and the launch cannot hold two opinions of one account.
//
// automatic says the rule will choose. Only then are credentials read: they
// are the evidence that a login has expired, which matters to a choice made
// for the person and not to a launch on an account they named — launching a
// logged-out account by name is how one logs in.
//
// An account is excluded from an automatic choice only on positive evidence:
// its identity document parsed and names nobody, its refresh token is
// demonstrably expired, or the vendor says it is blocked. A credential that
// could not be read excludes nothing. With a locked Keychain every account
// reads that way, and in auto mode a usable account must never be refused for
// want of bookkeeping. The vendor's health probe is not consulted: it can say
// "logged in" over a refresh token that has expired, and a launch has only
// the credential to go by.
//
// Busy sessions are the subscription's, not the dir's: two dirs on one
// account, in this home or in another home sharing the ledger, spend one
// quota, so every verified-live busy session on that account counts on each
// candidate that spends it — and so does every headless run whose process
// still runs.
func buildCandidates(set accounts.Set, src []placeSource, env []string, automatic bool, now time.Time, home string, others []accounts.Home, runs []state.Run) placeFacts {
	scope := set.Scope
	f := placeFacts{set: set, home: home, prepared: map[string]launch.Prepared{}, prepareErr: map[string]error{}}

	var blobs []string
	if automatic && scope.Vendor == config.Claude {
		accts := make([]accounts.Account, len(src))
		for i, s := range src {
			accts[i] = s.acct
		}
		blobs = readCredsParallel(accts)
	}
	keys := make(map[string]string, len(src))
	for _, s := range src {
		keys[s.acct.Name] = s.key.ID()
	}
	load := readSessionLoad(set, keys, home, others, runs)
	f.refs, f.evidence, f.gone = load.refs, load.evidence, load.gone

	for i, s := range src {
		a := s.acct
		c := placement.Candidate{Name: a.Name, Key: s.key.ID(), Busy: load.busy[s.key.ID()], Statuses: load.statuses[s.key.ID()]}
		raw := ""
		if blobs != nil {
			raw = blobs[i]
		}
		if p, err := launch.Prepare(a, set, env); err != nil {
			f.prepareErr[a.Name] = err
			c.Excluded, c.Unlaunchable = err.Error(), true
		} else {
			f.prepared[a.Name] = p
			c.Excluded = avoidReason(a, s.obs, raw, now)
		}
		c.ObservedAt, c.Source, c.Limits = limitsOf(scope.Vendor, s.obs)
		f.cands = append(f.cands, c)
		f.keys = append(f.keys, s.key)
		f.obs = append(f.obs, s.obs)
		askable := false
		switch {
		case !automatic:
		case scope.Vendor == config.Codex:
			askable = a.Auth.State == codexauth.OK && !a.Auth.TokenStale(now.Unix())
		default:
			blob, ok := creds.Parse(raw)
			askable = ok && a.Readable && blob.TokenUsable(now.UnixMilli())
		}
		f.askable = append(f.askable, askable)
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
// homes', and samples each pid once for all of them. keys maps this home's
// account names to their subscription keys; another home's account counts
// only when its identity says which subscription it is, since its dir name is
// that home's to choose. Codex has no registry headroom can read: its accounts
// carry no busy sessions, and an empty read there is not evidence of idleness.
func readSessionLoad(set accounts.Set, keys map[string]string, home string, others []accounts.Home, runs []state.Run) sessionLoad {
	out := sessionLoad{busy: map[string][]placement.Proc{}, statuses: map[string][]string{}}
	if set.Scope.Vendor != config.Claude {
		return out
	}
	type claimant struct{ key, home string }
	who := map[string]claimant{}
	var own []sessions.RegistryEntry
	for _, a := range set.Accounts {
		out.refs = append(out.refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir()})
		who[a.Name] = claimant{keys[a.Name], home}
		own = append(own, sessions.ReadRegistry(a.Name, a.Dir()).Entries...)
	}
	all := append([]sessions.RegistryEntry(nil), own...)
	for _, o := range others {
		for _, a := range o.Set.Accounts {
			if a.AccountID == "" {
				continue
			}
			// A label no account of this home can carry: the registry reader
			// stamps it on every entry, and it is mapped back here.
			label := o.Root + "\x00" + a.Name
			who[label] = claimant{state.KeyFor(a).ID(), o.Root}
			all = append(all, sessions.ReadRegistry(label, a.Dir()).Entries...)
		}
	}
	for i, r := range runs {
		// Labelled like another home's accounts, mapped back below.
		label := fmt.Sprintf("\x00run\x00%d", i)
		who[label] = claimant{r.Key, r.Home}
		all = append(all, runEntry(label, r))
	}
	probe := sampleOnce(placementProbe)
	out.evidence, _ = sessions.InspectClaims(own, probe)
	evidence, live := sessions.InspectClaims(all, probe)
	for _, r := range runs {
		if evidence[runEntry("", r).SessionID].State == sessions.NotLive {
			out.gone = append(out.gone, r)
		}
	}
	for _, e := range live {
		c := who[e.Account]
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

// runEntry is a headless run as a registry claim: working, for as long as
// its process runs.
func runEntry(label string, r state.Run) sessions.RegistryEntry {
	return sessions.RegistryEntry{
		Account: label, SessionID: "run:" + r.Placement, PID: r.PID, StartedAtMS: r.StartedMS,
		OK: true, Status: sessions.StatusBusy, StatusState: tag.OK,
	}
}

// sampleOnce answers each pid from one sample, however many times it is
// asked: liveness and ownership read the same process table.
func sampleOnce(probe sessions.PIDProbe) sessions.PIDProbe {
	type sample struct {
		start int64
		err   error
	}
	seen := map[int]sample{}
	return func(pid int) (int64, error) {
		if s, ok := seen[pid]; ok {
			return s.start, s.err
		}
		start, err := probe(pid)
		seen[pid] = sample{start, err}
		return start, err
	}
}

// limitsOf turns an observation into the rows the rule counts: the ones that
// bound ordinary work on the account, with the session window marked. Which
// rows those are is the usage package's to say.
func limitsOf(vendor config.Vendor, obs *accountstate.Observation) (observedAt int64, source string, limits []placement.Limit) {
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
	return obs.ObservedAt, sourceNames[obs.Source], limits
}

// markPlacement sets, on every row, the load an automatic launch would count
// on that account — its busy sessions and recent launches, from every home
// sharing the ledger — and, under auto, Next on the row such a launch would
// take from the figures the board is showing. mode is the board's own reading
// of `.current`, taken once with its facts; outside auto nothing is marked.
// The mark is advice: it is computed from this surface's observations and the
// record as it stood, and a launch decides again from the disk — counting
// whatever was placed in between. What it is not allowed to be is a second
// opinion: the candidates come from buildCandidates, the builder a launch
// uses.
//
// It returns what it gathered, so a surface that stays open can mark again
// as time passes without gathering again; nil when there was nothing to mark.
func markPlacement(set accounts.Set, list []*accountData, mode string, st *state.Store, now time.Time) *placeMark {
	for _, d := range list {
		d.View.Next, d.View.Load = false, nil
	}
	if len(list) == 0 || st == nil {
		return nil
	}
	snap := st.Load()
	src := make([]placeSource, len(list))
	for i, d := range list {
		src[i] = placeSource{acct: d.Acct, key: d.Key, obs: d.View.Obs}
	}
	auto := mode == "auto"
	f := buildCandidates(set, src, os.Environ(), auto, now, st.Home(), otherHomes(set.Scope, snap), snap.Runs())
	m := &placeMark{cands: f.cands, ledger: snap.Placements(), home: st.Home(), labels: homeLabels(snap, st.Home()), auto: auto}
	m.mark(list, now)
	return m
}

// placeMark is what one gathering for a board's marks read: the candidates
// and the record of launches. The choice they decide moves with the clock
// alone — a window ends, a launch stops counting as load, a weekly reset comes
// nearer and reorders a tie — so a page that stays open marks again from them
// at every frame's time. What they were read from — credentials, live
// sessions, the record — is as fresh as the last round, and a launch made from
// another terminal since then shows at the next.
type placeMark struct {
	cands  []placement.Candidate
	ledger placement.Ledger
	home   string
	labels map[string]string
	auto   bool
}

// mark sets every row's load and, under auto, Next on the row a launch would
// take at now.
func (m *placeMark) mark(list []*accountData, now time.Time) {
	decision := placement.Choose(m.cands, m.ledger, placement.Intent{Home: m.home}, now)
	for _, d := range list {
		d.View.Next, d.View.Load = false, nil
		if c, ok := decision.Find(d.Acct.Name); ok {
			d.View.Load = loadFacts(c, m.home, m.labels)
		}
		d.View.Next = m.auto && decision.Chosen != "" && d.Acct.Name == decision.Chosen
	}
}

// loadFacts is a counted candidate's load as a surface reports it.
func loadFacts(c placement.Counted, self string, labels map[string]string) *accountstate.Load {
	l := &accountstate.Load{Value: c.Load, Busy: c.Busy, Launched: c.Pending}
	for _, sh := range c.Shares {
		l.Homes = append(l.Homes, accountstate.HomeLoad{
			Home: sh.Home, Label: homeLabel(labels, sh.Home), This: sh.Home == self, Busy: sh.Busy, Launched: sh.Pending,
		})
	}
	return l
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
// Positive evidence only — see buildCandidates.
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
func readCredsParallel(accts []accounts.Account) []string {
	out := make([]string, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a accounts.Account) {
			defer wg.Done()
			out[i] = placementCreds(a)
		}(i, a)
	}
	wg.Wait()
	return out
}
