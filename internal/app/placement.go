package app

// What a launch reads before it chooses an account, and how the choice is
// said. Reading is all this file does to the world: preparation, discovery,
// the stored observations, the session registry and — for an automatic choice
// — the credentials. Nothing here asks the network; figures are refreshed for
// the *next* launch by a detached round (startRefresh), so the command typed
// most never waits on an endpoint.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	placementCreds = func(a accounts.Account) string { return creds.ReadRaw(a.ConfigDir) }
	// placementProbe answers when a pid started, for registry liveness.
	placementProbe sessions.PIDProbe = psProbe
)

// placeFacts is everything one launch read. The discovered set travels with
// what was built from it, as everywhere else.
type placeFacts struct {
	set   accounts.Set
	snap  state.Snapshot
	cands []placement.Candidate
	keys  []state.Key // the ledger key of each candidate, in order

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

// gatherPlacement reads what is known about every account of the set.
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
// want of bookkeeping.
func gatherPlacement(set accounts.Set, st *state.Store, env []string, automatic bool, now time.Time) placeFacts {
	scope := set.Scope
	f := placeFacts{
		set: set, snap: st.Load(),
		prepared: map[string]launch.Prepared{}, prepareErr: map[string]error{},
	}
	facts, _ := accountstate.Assemble(set, f.snap, now)

	var blobs []string
	if automatic && scope.Vendor == config.Claude {
		blobs = readCredsParallel(set.Accounts)
	}

	busy := map[string][]placement.Proc{}
	statuses := map[string][]string{}
	if scope.Vendor == config.Claude {
		// Codex has no registry headroom can read; its accounts carry no busy
		// sessions, and an empty read there is not evidence of idleness.
		var entries []sessions.RegistryEntry
		for _, a := range set.Accounts {
			f.refs = append(f.refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir()})
			entries = append(entries, sessions.ReadRegistry(a.Name, a.Dir()).Entries...)
		}
		var live []sessions.RegistryEntry
		f.evidence, live = sessions.InspectClaims(entries, placementProbe)
		for _, e := range live {
			statuses[e.Account] = append(statuses[e.Account], statusWord(e))
			if e.Busy() {
				busy[e.Account] = append(busy[e.Account], placement.Proc{PID: e.PID, StartedMS: e.StartedAtMS})
			}
		}
	}

	for i, fa := range facts {
		a := fa.Acct
		c := placement.Candidate{Name: a.Name, Key: fa.Key.ID(), Busy: busy[a.Name], Statuses: statuses[a.Name]}
		if p, err := launch.Prepare(a, set, env); err != nil {
			f.prepareErr[a.Name] = err
			c.Excluded, c.Unlaunchable = err.Error(), true
		} else {
			f.prepared[a.Name] = p
			raw := ""
			if blobs != nil {
				raw = blobs[i]
			}
			c.Excluded = avoidReason(fa, raw, now)
		}
		if obs := fa.View.Obs; obs != nil {
			c.ObservedAt, c.Source = obs.ObservedAt, sourceNames[obs.Source]
			session := usage.SessionWindow(scope.Vendor, obs.Rows)
			for j, r := range obs.Rows {
				if !usage.General(scope.Vendor, r) {
					continue
				}
				c.Limits = append(c.Limits, placement.Limit{
					Kind: r.Kind, Label: r.Label, Percent: r.Percent,
					Bad: r.PercentState == usage.StateBad, ResetAt: r.ResetAt, Session: j == session,
				})
			}
		}
		f.cands = append(f.cands, c)
		f.keys = append(f.keys, fa.Key)
	}
	return f
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
// Positive evidence only — see gatherPlacement.
func avoidReason(fa accountstate.Account, raw string, now time.Time) string {
	a := fa.Acct
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
	if fa.View.Blocked() {
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
	cmd := exec.Command(exe, "refresh", "--vendor", string(scope.Vendor))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// refreshWorthStarting reports whether any account may be asked right now.
// The claim still decides; this only keeps a launch from spawning a process
// that would be refused on every account.
func refreshWorthStarting(f placeFacts, now time.Time) bool {
	for _, k := range f.keys {
		if !f.snap.NextEligible(k, now).After(now) {
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

// ---- saying the choice ----

// launchLine is the one line a launch prints before the vendor starts: the
// account, how it was decided, and the figures that decided it. mode is how
// the account came to be decided; ref is the session the launch named.
func launchLine(d placement.Decision, mode string, ref sessionRef, owner string, now time.Time) string {
	parts := []string{d.Chosen}
	c, _ := d.Find(d.Chosen)
	switch mode {
	case "pinned":
		return d.Chosen + " · pinned (a on the board turns auto on)"
	case "last":
		return d.Chosen + " · the last account used"
	}
	parts = append(parts, "auto")
	switch d.Reason {
	case placement.ReasonOwner:
		parts = append(parts, "this session's account")
	case placement.ReasonMoved:
		if owner != "" {
			parts = append(parts, "session moved from "+owner)
		} else {
			parts = append(parts, "session had no known account")
		}
	case placement.ReasonNearLimit:
		near := "every account is near a limit · " + shortLabel(c.HighestLabel) + fmt.Sprintf(" %d%%", c.Highest)
		if c.HighestReset > now.Unix() {
			near += ", resets in " + render.Remaining(c.HighestReset-now.Unix())
		}
		parts = append(parts, near)
	}
	if ref.Unidentified {
		parts = append(parts, "session not identified")
	}
	if d.Reason != placement.ReasonNearLimit {
		parts = append(parts, figures(c, now)...)
		parts = append(parts, fmt.Sprintf("load %d", c.Load))
	}
	line := strings.Join(parts, " · ")
	if d.RunnerUp != "" {
		line += " (next: " + d.RunnerUp + ")"
	}
	return line
}

// figures is one account's counted session and weekly figures, and how far
// they can be trusted: an old observation's are lower bounds and say so.
func figures(c placement.Counted, now time.Time) []string {
	if c.ObservedAt == 0 {
		return []string{"never observed"}
	}
	var out []string
	if s, ok := c.Session(); ok {
		out = append(out, fmt.Sprintf("%s %d%%", shortLabel(s.Label), s.Counted))
	}
	if len(c.Limits) > 1 || (len(c.Limits) == 1 && !c.Limits[0].Session) {
		out = append(out, fmt.Sprintf("week %d%%", c.Weekly))
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
			session = fmt.Sprintf("%d%%", s.Counted)
		}
		if c.ObservedAt > 0 && len(c.Limits) > 0 {
			week = fmt.Sprintf("%d%%", c.Weekly)
		}
		fmt.Fprintf(w, "%s%s  %7s  %5s  %4d  %7d  %4d  %s\n", mark, render.PadCell(render.Sanitize(c.Name), nameW),
			session, week, c.Busy, c.Pending, c.Load, dryRunNote(c, now))
	}
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
