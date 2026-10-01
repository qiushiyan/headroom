package app

// What a launch reads before it chooses an account, and how the choice is
// said. Reading is all this file does to the world: preparation, discovery,
// the stored observations, the session registry and — for an automatic choice
// — the credentials. Nothing here asks the network; figures are refreshed for
// the *next* launch by a detached round (startRefresh), so the command typed
// most never waits on an endpoint.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	f := buildCandidates(set, src, env, automatic, now)
	f.snap = snap
	return f
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
func buildCandidates(set accounts.Set, src []placeSource, env []string, automatic bool, now time.Time) placeFacts {
	scope := set.Scope
	f := placeFacts{set: set, prepared: map[string]launch.Prepared{}, prepareErr: map[string]error{}}

	var blobs []string
	if automatic && scope.Vendor == config.Claude {
		accts := make([]accounts.Account, len(src))
		for i, s := range src {
			accts[i] = s.acct
		}
		blobs = readCredsParallel(accts)
	}
	load := readSessionLoad(set)
	f.refs, f.evidence = load.refs, load.evidence

	for i, s := range src {
		a := s.acct
		c := placement.Candidate{Name: a.Name, Key: s.key.ID(), Busy: load.busy[a.Name], Statuses: load.statuses[a.Name]}
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

// sessionLoad is what the vendor's registry says is running right now: per
// account, the sessions it reports as working and every live session's status
// word, each from a claim verified by its own process.
type sessionLoad struct {
	busy     map[string][]placement.Proc
	statuses map[string][]string
	refs     []sessions.AccountRef
	evidence map[string]sessions.ProcessEvidence
}

// readSessionLoad reads every account's registry and samples each pid once.
// Codex has no registry headroom can read: its accounts carry no busy
// sessions, and an empty read there is not evidence of idleness.
func readSessionLoad(set accounts.Set) sessionLoad {
	out := sessionLoad{busy: map[string][]placement.Proc{}, statuses: map[string][]string{}}
	if set.Scope.Vendor != config.Claude {
		return out
	}
	var entries []sessions.RegistryEntry
	for _, a := range set.Accounts {
		out.refs = append(out.refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir()})
		entries = append(entries, sessions.ReadRegistry(a.Name, a.Dir()).Entries...)
	}
	var live []sessions.RegistryEntry
	out.evidence, live = sessions.InspectClaims(entries, placementProbe)
	for _, e := range live {
		out.statuses[e.Account] = append(out.statuses[e.Account], statusWord(e))
		if e.Busy() {
			out.busy[e.Account] = append(out.busy[e.Account], placement.Proc{PID: e.PID, StartedMS: e.StartedAtMS})
		}
	}
	return out
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
			Bad: r.PercentState == usage.StateBad, ResetAt: r.ResetAt, Session: j == session,
		})
	}
	return obs.ObservedAt, sourceNames[obs.Source], limits
}

// markNext sets Next on the row an automatic launch would take from the
// figures a board is showing, and clears it everywhere else. mode is the
// board's own reading of `.current`, taken once with its facts; outside auto
// nothing is marked. The mark is advice: it is computed from this surface's
// observations and the record as it stood, and a launch decides again from
// the disk — counting whatever was placed in between. What it is not allowed
// to be is a second opinion: the candidates come from buildCandidates, the
// builder a launch uses.
func markNext(set accounts.Set, list []*accountData, mode string, ledger placement.Ledger, now time.Time) {
	for _, d := range list {
		d.View.Next = false
	}
	if mode != "auto" || len(list) == 0 {
		return
	}
	src := make([]placeSource, len(list))
	for i, d := range list {
		src[i] = placeSource{acct: d.Acct, key: d.Key, obs: d.View.Obs}
	}
	f := buildCandidates(set, src, os.Environ(), true, now)
	chosen := placement.Choose(f.cands, ledger, placement.Intent{}, now).Chosen
	for _, d := range list {
		d.View.Next = chosen != "" && d.Acct.Name == chosen
	}
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

// runRefresh is the round with nobody watching: ask about every account the
// claim permits, record the answers, print nothing. It is what an automatic
// launch leaves running behind it.
func runRefresh(scopes []config.Scope) int {
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Add(1)
		go func(scope config.Scope) {
			defer wg.Done()
			st := state.Open(scope)
			p := prepareUnprobed(scope, st)
			for range launchFetches(context.Background(), p.list, st) {
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
	live       state.Enumerator

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
}

// placeLaunch decides the account, records the launch and whatever it means
// for the session in one store operation, and appends the log line. An error
// is a refusal — nothing was recorded and nothing may start. Bookkeeping that
// merely failed is a note on the outcome, never a refusal: in auto mode every
// usable account is a correct answer.
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
	placed, placeErr := req.st.Place(state.Launch{
		Candidates: facts.cands, Intent: req.intent, PID: os.Getpid(), Now: req.now,
		Session: req.ref.Record, MustReHome: req.mustReHome, Live: req.live,
	})
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

	if placeErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", placeErr))
	}
	if placed.SessionErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("session routing not recorded (%v) — run headroom check", placed.SessionErr))
	}
	rec := launchlog.New(out.decision, req.now)
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = string(scope.Vendor), os.Getpid(), req.cwd, req.mode, placed.Recorded
	rec.Session = req.ref.Route
	if placeErr != nil {
		rec.Problem = placeErr.Error()
	}
	if err := launchlog.Append(scope.AccountsRoot, rec); err != nil {
		out.notes = append(out.notes, fmt.Sprintf("launch log not written (%v)", err))
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
	fmt.Fprintln(w, prefix+launchLine(o.decision, o.req.intent.Kind == placement.Auto, o.req.ref, o.req.owner, o.req.now))
	for _, note := range o.notes {
		fmt.Fprintln(w, prefix+note)
	}
}

// ---- saying the choice ----

// launchLine is the one line a launch prints before the vendor starts: the
// account, how it was decided, and — when the rule decided — the figures it
// decided on, each said as what it is. automatic says the rule chose; ref and
// owner are the session the launch is for and the account that last drove it.
func launchLine(d placement.Decision, automatic bool, ref sessionRef, owner string, now time.Time) string {
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
		parts = append(parts, fmt.Sprintf("load %d", c.Load))
	}
	line := strings.Join(parts, " · ")
	if d.RunnerUp != "" {
		line += " (next: " + d.RunnerUp + ")"
	}
	return line
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
	if c.WeeklyBasis != "" {
		out = append(out, "week "+figure(c.Weekly, c.WeeklyBasis))
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
// account, with what was counted and why an account was left out.
func writeDryRun(w io.Writer, vendor config.Vendor, d placement.Decision, mode string, now time.Time) {
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
	fmt.Fprintf(w, "  %s  %7s  %5s  %4s  %7s  %4s  %s\n", render.PadCell("account", nameW), "session", "week", "busy", "pending", "load", "note")
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
		if c.WeeklyBasis != "" {
			week = cell(c.Weekly, c.WeeklyBasis)
		}
		fmt.Fprintf(w, "%s%s  %7s  %5s  %4d  %7d  %4d  %s\n", mark, render.PadCell(render.Sanitize(c.Name), nameW),
			session, week, c.Busy, c.Pending, c.Load, dryRunNote(c, now))
	}
}

// cell is figure for a table column: the same distinctions, in fewer cells.
func cell(counted int, basis placement.Basis) string {
	if basis == placement.BasisEnded {
		return "ended"
	}
	return figure(counted, basis)
}

func dryRunNote(c placement.Counted, now time.Time) string {
	var notes []string
	if c.Excluded != "" {
		notes = append(notes, "excluded: "+c.Excluded)
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
