package app

// What a launch reads before it chooses an account, and how the choice is
// said. Reading is all this file does to the world: preparation, discovery,
// the stored observations, the session registry and — for an automatic choice
// — the credentials. Nothing here asks the network; figures are refreshed for
// the *next* launch by a detached round (startRefresh), so the command typed
// most never waits on an endpoint.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
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
	f := buildCandidates(set, src, env, automatic, now, st.Home(), otherHomes(set.Scope, snap))
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
	return accounts.OtherHomes(scope, registered(scope.Vendor, snap))
}

// registered is every home the ledger has a registration for, as a scope to
// discover it under.
func registered(vendor config.Vendor, snap state.Snapshot) []config.Scope {
	var out []config.Scope
	for _, m := range snap.Members() {
		out = append(out, config.HomeScope(vendor, m.Home, m.Root, m.Explicit))
	}
	return out
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
// candidate that spends it.
func buildCandidates(set accounts.Set, src []placeSource, env []string, automatic bool, now time.Time, home string, others []accounts.Home) placeFacts {
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
	load := readSessionLoad(set, keys, home, others)
	f.refs, f.evidence = load.refs, load.evidence

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
type sessionLoad struct {
	busy     map[string][]placement.Proc
	statuses map[string][]string
	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence
}

// readSessionLoad reads every account's registry, this home's and the other
// homes', and samples each pid once for all of them. keys maps this home's
// account names to their subscription keys; another home's account counts
// only when its identity says which subscription it is, since its dir name is
// that home's to choose. Codex has no registry headroom can read: its accounts
// carry no busy sessions, and an empty read there is not evidence of idleness.
func readSessionLoad(set accounts.Set, keys map[string]string, home string, others []accounts.Home) sessionLoad {
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
			who[label] = claimant{state.Key{UUID: a.AccountID, Name: a.Name}.ID(), o.Root}
			all = append(all, sessions.ReadRegistry(label, a.Dir()).Entries...)
		}
	}
	probe := sampleOnce(placementProbe)
	out.evidence, _ = sessions.InspectClaims(own, probe)
	_, live := sessions.InspectClaims(all, probe)
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
			Bad: r.PercentState == usage.StateBad, ResetAt: r.ResetAt, Window: r.WindowSeconds,
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
	f := buildCandidates(set, src, os.Environ(), auto, now, st.Home(), otherHomes(set.Scope, snap))
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

// startRefresh starts one detached refresh round for the scope's vendor and
// returns at once. The launching process is about to be replaced by the
// vendor, so it cannot wait for a response — and a request whose answer nobody
// stays to record would spend the account's budget for nothing. The child is
// an ordinary headroom running the ordinary round: the claim authorizes, the
// completion records, and it has no terminal to disturb.
var startRefresh = func(scope config.Scope) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.HasSuffix(filepath.Base(exe), ".test") {
		// A test binary re-executed with these arguments would run the suite
		// again, which would launch again. The tests that mean to observe this
		// edge replace it; one that forgot must not be able to fork.
		return nil
	}
	cmd := exec.Command(exe, "refresh", "--vendor", string(scope.Vendor))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// refreshWorthStarting reports whether any account could be asked right now:
// its quiet period has passed and its stored token is usable. The claim still
// decides; this only keeps a launch from spawning a process that could ask
// nobody — which, on a machine whose idle accounts' tokens have aged out,
// would otherwise be every launch.
func refreshWorthStarting(f placeFacts, now time.Time) bool {
	for i, k := range f.keys {
		if i < len(f.askable) && f.askable[i] && !f.snap.NextEligible(k, now).After(now) {
			return true
		}
	}
	return false
}

// refreshDeadline bounds the round a launch leaves behind: the claim, and a
// fetch the HTTP client already gives up on after ten seconds. Completion
// follows it either way. The round lives inside whatever launched it — an
// automated job that is torn down when it ends counts it as its own process —
// so it must be short, and must stop cleanly when told to.
const refreshDeadline = 12 * time.Second

// runRefresh is the round with nobody watching: ask about every account the
// claim permits, record the answers, print nothing. It is what an automatic
// launch leaves running behind it.
//
// SIGTERM, SIGINT and SIGHUP end it early and cleanly, whatever it is doing:
// a credential read in progress is killed, requests in flight are abandoned
// and recorded as abandoned, an answer that already arrived is kept, and
// nothing is claimed once the signal has come. It is then gone within
// about a second — the lock a completion takes is held for milliseconds — so
// a supervisor's TERM-then-KILL never has to reach the KILL. Killed outright
// mid-request, it leaves a claim that simply expires at its spacing: a claim
// is a reservation with a deadline, which no reader waits on, and the lock
// dies with the process.
func runRefresh(scopes []config.Scope) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, refreshDeadline)
	defer cancel()
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Add(1)
		go func(scope config.Scope) {
			defer wg.Done()
			st := state.Open(scope)
			p := prepareUnprobed(ctx, scope, st)
			for range launchFetches(ctx, p.list, st) {
			}
		}(scope)
	}
	wg.Wait()
	return 0
}

// ---- the launch itself ----

// launchRequest is what a surface asks for: how the account is to be decided,
// and what the launch means for a session. `launch` and the session picker
// both ask this way, so a launch is placed, recorded, logged and announced by
// one operation whichever surface started it.
type launchRequest struct {
	st     *state.Store
	intent placement.Intent
	mode   string // how the account came to be decided: auto, pinned, named, last, picker

	// ref is the session the launch is for. Its Record id is re-homed when the
	// launch puts it somewhere other than its owner; mustReHome says the
	// launch exists to move it, and is refused if that cannot be recorded.
	ref        sessionRef
	owner      string
	mustReHome bool

	cwd string
	now time.Time
}

// launchOutcome is a launch that has been decided and recorded and has not
// yet replaced this process.
type launchOutcome struct {
	req      launchRequest
	decision placement.Decision
	account  accounts.Account
	prepared launch.Prepared

	// notes are the bookkeeping that did not happen, worded for the person:
	// the launch proceeds regardless, and says so.
	notes []string

	// home and labels say whose load is whose: this home's accounts root,
	// and every home's name.
	home   string
	labels map[string]string
}

// placeLaunch decides the account, records the launch and whatever it means
// for the session in one store operation, and appends the log line. An error
// is a refusal — nothing was recorded and nothing may start. Bookkeeping that
// merely failed is a note on the outcome, never a refusal: in auto mode every
// usable account is a correct answer.
//
// What a launch owes the next one is decided here too, not by the surface:
// a launch that writes a re-home sweeps the ones whose transcripts are gone,
// and a launch the rule placed leaves a refresh behind it. A surface that had
// to remember either would be a surface that forgot one. What stays with the
// surface is its terminal, its working directory and the exec.
func placeLaunch(req launchRequest, facts placeFacts) (launchOutcome, error) {
	out := launchOutcome{req: req}
	scope := facts.set.Scope
	if req.intent.Kind == placement.Forced {
		// The account was named — by flag, by pin, or as a session's owner:
		// its own preparation error is the answer, said the way it always was.
		if err := facts.prepareErr[req.intent.Account]; err != nil {
			return out, err
		}
	}
	launch := state.Launch{
		Candidates: facts.cands, Intent: req.intent, PID: os.Getpid(), Now: req.now,
		Session: req.ref.Record, MustReHome: req.mustReHome,
	}
	if req.ref.Record != "" {
		launch.Live = func() (map[string]bool, bool) { return sessions.TranscriptIDs(scope.StoreDir()) }
	}
	placed, placeErr := req.st.Place(launch)
	if req.mustReHome && placeErr != nil {
		return out, fmt.Errorf("re-home not recorded (%v) — enter resumes without it", placeErr)
	}
	out.decision = placed.Decision
	if out.decision.Chosen == "" {
		return out, errors.New(out.decision.Refusal)
	}
	prepared, ok := facts.prepared[out.decision.Chosen]
	acct, found := facts.account(out.decision.Chosen)
	if !ok || !found {
		if err := facts.prepareErr[out.decision.Chosen]; err != nil {
			return out, err
		}
		return out, fmt.Errorf("%s cannot be launched", out.decision.Chosen)
	}
	out.prepared, out.account = prepared, acct
	out.home, out.labels = facts.home, homeLabels(facts.snap, facts.home)

	switch {
	case placeErr != nil:
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", placeErr))
	case placed.RecordErr != nil:
		// The re-home this launch depended on is written; the shared ledger's
		// write failed after it.
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", placed.RecordErr))
	}
	if placed.SessionErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("session routing not recorded (%v) — run headroom check", placed.SessionErr))
	}
	rec := launchlog.New(out.decision, req.now)
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = string(scope.Vendor), os.Getpid(), req.cwd, req.mode, placed.Recorded
	rec.Session = req.ref.Route
	if err := cmp.Or(placeErr, placed.RecordErr); err != nil {
		rec.Problem = err.Error()
	}
	if err := launchlog.Append(scope.AccountsRoot, rec); err != nil {
		out.notes = append(out.notes, fmt.Sprintf("launch log not written (%v)", err))
	}
	if req.intent.Kind == placement.Auto && refreshWorthStarting(facts, req.now) {
		// For the next launch, not this one: nothing here waits on a network.
		_ = startRefresh(scope)
	}
	return out, nil
}

// announce says, before the vendor starts, where the launch is going and why,
// and what bookkeeping was missed on the way. Every launch says it, whichever
// surface started it and however the account was decided: a line that appears
// for some launches and not others is one a person stops reading.
func (o launchOutcome) announce(w io.Writer, prefix string) {
	for _, notice := range o.prepared.Notices {
		fmt.Fprintln(w, prefix+notice)
	}
	c, _ := o.decision.Find(o.decision.Chosen)
	fmt.Fprintln(w, prefix+launchLine(o.decision, o.req.intent.Kind == placement.Auto, o.req.ref, o.req.owner, elsewhere(c, o.home, o.labels), o.req.now))
	for _, note := range o.notes {
		fmt.Fprintln(w, prefix+note)
	}
}

// ---- saying the choice ----

// launchLine is the one line a launch prints before the vendor starts: the
// account, how it was decided, and — when the rule decided — the figures it
// decided on, each said as what it is. automatic says the rule chose; ref and
// owner are the session the launch is for and the account that last drove it;
// others is how much of the chosen account's load other homes put there.
func launchLine(d placement.Decision, automatic bool, ref sessionRef, owner, others string, now time.Time) string {
	parts := []string{d.Chosen}
	c, _ := d.Find(d.Chosen)
	if !automatic {
		switch d.Reason {
		case "pinned":
			parts = append(parts, "pinned (a on the board turns auto on)")
		case placement.ReasonLast:
			parts = append(parts, "the last account used")
		case placement.ReasonOwner:
			parts = append(parts, "this session's account")
		default:
			parts = append(parts, d.Reason)
		}
		return strings.Join(parts, " · ")
	}
	parts = append(parts, "auto")
	switch {
	case ref.Unidentified:
		parts = append(parts, "session not identified")
	case ref.Route == "":
	case owner == "":
		parts = append(parts, "session had no known account")
	case owner == d.Chosen:
		parts = append(parts, "this session's account")
	default:
		parts = append(parts, "session moved from "+owner)
	}
	if d.Reason == placement.ReasonNearLimit {
		near := "every account is near a limit · " + shortLabel(c.HighestLabel) + " " + figure(c.Highest, c.HighestBasis)
		if c.HighestReset > now.Unix() {
			near += ", resets in " + render.Remaining(c.HighestReset-now.Unix())
		}
		parts = append(parts, near)
		if c.Stale {
			parts = append(parts, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old")
		}
	} else {
		parts = append(parts, figures(c, now)...)
		load := fmt.Sprintf("load %d", c.Load)
		if others != "" {
			load += " (" + others + ")"
		}
		parts = append(parts, load)
	}
	line := strings.Join(parts, " · ")
	if d.RunnerUp != "" {
		line += " (next: " + d.RunnerUp + ")"
	}
	return line
}

// elsewhere says how much of a counted account's busy sessions and recent
// launches came from homes other than this one, by name: "2 from
// steward-home". "" when none did.
func elsewhere(c placement.Counted, home string, labels map[string]string) string {
	var parts []string
	for _, sh := range c.Elsewhere(home) {
		if n := sh.Busy + sh.Pending; n > 0 {
			parts = append(parts, fmt.Sprintf("%d from %s", n, render.Sanitize(homeLabel(labels, sh.Home))))
		}
	}
	return strings.Join(parts, ", ")
}

// figure is one counted percentage said as what it rests on: measured, a lower
// bound from an old observation, a window that has since ended, or a figure
// that did not parse. A bound printed as a bare percent reads as measured.
func figure(counted int, basis placement.Basis) string {
	switch basis {
	case placement.BasisStale:
		return fmt.Sprintf("≥%d%%", counted)
	case placement.BasisEnded:
		return "window ended"
	case placement.BasisBad:
		return "?%"
	default:
		return fmt.Sprintf("%d%%", counted)
	}
}

// figures is one account's session and weekly figures, and how old they are
// when that matters.
func figures(c placement.Counted, now time.Time) []string {
	if c.ObservedAt == 0 {
		return []string{"never observed"}
	}
	var out []string
	if s, ok := c.Session(); ok {
		out = append(out, shortLabel(s.Label)+" "+figure(s.Counted, s.Basis))
	}
	if c.Week.Basis != "" {
		week := "week " + figure(c.Week.Counted, c.Week.Basis)
		if c.Week.LeftBasis == placement.TimeReset {
			week += ", resets in " + render.Remaining(c.Week.Left)
		}
		out = append(out, week)
	}
	if len(c.Limits) == 0 {
		out = append(out, "no limits reported")
	}
	if c.Stale {
		out = append(out, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old")
	}
	return out
}

// shortLabel trims a row's label to what fits a one-line launch message.
func shortLabel(label string) string {
	label = strings.TrimSuffix(render.Sanitize(label), " session")
	if label == "" {
		return "limit"
	}
	return label
}

// writeDryRun prints the whole table a launch would choose from: one row per
// account, with what was counted and why an account was left out. busy and
// pending count every home sharing the ledger; a note says whose were not
// this home's.
func writeDryRun(w io.Writer, vendor config.Vendor, d placement.Decision, mode string, now time.Time, home string, labels map[string]string) {
	if d.Chosen == "" {
		fmt.Fprintf(w, "headroom launch --dry-run: would refuse — %s\n", d.Refusal)
	} else {
		how := mode
		if d.Reason != "" && d.Reason != mode {
			how += " · " + d.Reason
		}
		fmt.Fprintf(w, "headroom launch --dry-run: would start %s on %s (%s)\n", scopeBinary(vendor), d.Chosen, how)
	}
	fmt.Fprintf(w, "rule %s — nothing was recorded, logged or started\n\n", d.Rule)

	nameW := len("account")
	for _, c := range d.Candidates {
		nameW = max(nameW, render.Cells(render.Sanitize(c.Name)))
	}
	fmt.Fprintf(w, "  %s  %7s  %5s  %6s  %4s  %7s  %4s  %s\n", render.PadCell("account", nameW), "session", "week", "resets", "busy", "pending", "load", "note")
	for _, c := range d.Candidates {
		mark := "  "
		switch c.Name {
		case d.Chosen:
			mark = "→ "
		case d.RunnerUp:
			mark = "· "
		}
		session, week := "—", "—"
		if s, ok := c.Session(); ok {
			session = cell(s.Counted, s.Basis)
		}
		if c.Week.Basis != "" {
			week = cell(c.Week.Counted, c.Week.Basis)
		}
		fmt.Fprintf(w, "%s%s  %7s  %5s  %6s  %4d  %7d  %4d  %s\n", mark, render.PadCell(render.Sanitize(c.Name), nameW),
			session, week, weekLeft(c.Week), c.Busy, c.Pending, c.Load, dryRunNote(c, now, home, labels))
	}
}

// weekLeft is how long a counted week has left, said as what it rests on: the
// vendor's reset as a countdown, a renewal its schedule projects as an
// approximate one, and "—" for a window counted whole because nothing dates
// it — there is no instant to print.
func weekLeft(w placement.Week) string {
	switch w.LeftBasis {
	case placement.TimeReset:
		return render.Remaining(w.Left)
	case placement.TimeProjected:
		return "≈" + render.Remaining(w.Left)
	default:
		return "—"
	}
}

// cell is figure for a table column: the same distinctions, in fewer cells.
func cell(counted int, basis placement.Basis) string {
	if basis == placement.BasisEnded {
		return "ended"
	}
	return figure(counted, basis)
}

func dryRunNote(c placement.Counted, now time.Time, home string, labels map[string]string) string {
	var notes []string
	if c.Excluded != "" {
		notes = append(notes, "excluded: "+c.Excluded)
	}
	for _, sh := range c.Elsewhere(home) {
		notes = append(notes, fmt.Sprintf("%s: %d busy, %d pending", render.Sanitize(homeLabel(labels, sh.Home)), sh.Busy, sh.Pending))
	}
	if c.NearLimit {
		notes = append(notes, fmt.Sprintf("near a limit (%s %d%%)", shortLabel(c.HighestLabel), c.Highest))
	}
	switch {
	case c.ObservedAt == 0:
		notes = append(notes, "never observed")
	case c.Stale:
		notes = append(notes, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old — lower bounds")
	default:
		notes = append(notes, "observed "+render.Age(now.Unix()-c.ObservedAt)+" ago")
	}
	for _, l := range c.Limits {
		if l.Basis == placement.BasisEnded {
			notes = append(notes, shortLabel(l.Label)+" window ended")
		}
	}
	return strings.Join(notes, " · ")
}

func scopeBinary(v config.Vendor) string {
	if v == config.Codex {
		return "codex"
	}
	return "claude"
}
