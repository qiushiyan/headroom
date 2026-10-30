package launchop

// A headless placement: the automatic launch's rule over the accounts a
// headless caller may take, recorded as load the way a launch is, for a
// session the caller starts itself. Nothing here sets an environment or execs
// anything — there is no child — and which accounts the caller may take is the
// caller's own rule, handed in.

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// HeadlessRequest is what a headless caller asks for.
type HeadlessRequest struct {
	Set   accounts.Set
	Store *state.Store

	// Prefer is the account a session ran on last, followed while it has
	// room, so the session's prompt cache stays warm, as a launch follows the
	// owner of a session it names. "" for none.
	Prefer string
	// Exclude are the accounts the caller will not take — a session that
	// ended on an account's usage limit, a token the vendor refused — by
	// name, joined as candidates' exclusions so the rule decides among the
	// rest in the same locked step that records the choice.
	Exclude map[string]bool
	// Eligible is why the caller cannot take an account, or "": the caller's
	// own policy, in place of an ordinary launch's preparation.
	Eligible func(a accounts.Account, key state.Key) string

	// DryRun decides and records nothing.
	DryRun bool
	CWD    string
	Now    time.Time
}

// HeadlessPlacement is what a headless placement decided.
type HeadlessPlacement struct {
	Decision placement.Decision
	// ID is the placement's id, which `started` reports the session's process
	// against; "" when nothing was placed.
	ID string
	// Failed is why nothing could be placed: the reservations could not be
	// read, or a choice could not be recorded. A placement nobody can
	// see is a session the person's launches cannot steer around, so an
	// unrecorded one is not handed out: Decision then chooses nobody.
	Failed error
	// Notes are bookkeeping that did not happen, for the caller to read.
	Notes []string
}

// PlaceHeadless decides which account the caller's next session spends and,
// unless it is a dry run, records the decision as load and logs it. A choice
// is always a recorded one: when the record fails, nothing is placed.
func PlaceHeadless(req HeadlessRequest, deps Deps) HeadlessPlacement {
	st, now := req.Store, req.Now
	intent := placement.Intent{Kind: placement.Auto, CanWait: true, Owner: req.Prefer, Home: st.Home()}
	snap := st.Load()
	f := observe(req.Set, st, snap, diskSources(req.Set, snap, now), req.CWD, true, deps)
	for i := range f.cands {
		c := &f.cands[i]
		if req.Exclude[c.Name] {
			c.Excluded = "excluded by the caller"
			continue
		}
		c.Excluded = cmp.Or(req.Eligible(c.acct, c.key), c.keptOut())
	}

	var out HeadlessPlacement
	if f.reservedErr != nil {
		out.Failed = reservedRefusal(f.reservedErr)
		out.Decision = placement.Decision{Rule: placement.Rule, Refusal: out.Failed.Error()}
		return out
	}
	if req.DryRun {
		out.Decision = placement.Choose(f.rule(), snap.Placements(), intent, now)
		return out
	}
	id := newPlacementID()
	placed, err := st.Place(state.Launch{Candidates: f.rule(), Intent: intent, Now: now, ID: id, Gone: f.gone})
	out.Decision = placed.Decision
	switch {
	case err != nil:
		out.Decision.Chosen, out.Decision.Refusal, out.Decision.RoomAt = "", fmt.Sprintf("the placement could not be recorded (%v)", err), 0
		out.Failed = err
	case out.Decision.Chosen != "":
		out.ID = id
		// The placement's line in the launch log, as a launch's: `launches
		// --eval` judges the rule by every decision it made.
		rec := launchlog.New(placed.Decision, f.rule(), placed.Ledger, intent, now)
		rec.Vendor, rec.Mode, rec.Recorded, rec.CWD = string(req.Set.Scope.Vendor), "headless", placed.Recorded, req.CWD
		if err := launchlog.Append(req.Set.Scope.AccountsRoot, rec); err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("launch log not written (%v)", err))
		}
		if worthRefreshing(f, now) {
			_ = deps.Refresh(req.Set.Scope)
		}
	}
	return out
}

// newPlacementID names one headless placement.
func newPlacementID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ErrExited is Started's answer for a process already gone: there is nothing
// more to count, and its placement still counts for its span.
var ErrExited = errors.New("process has already exited")

// Started records the process a caller started on a placement, so the session
// counts as busy on that account for as long as it runs — not only for the
// span a placement counts — and is counted once.
func Started(st *state.Store, id string, pid int, now time.Time, deps Deps) error {
	start, err := deps.Probe(pid)
	if errors.Is(err, os.ErrNotExist) {
		return ErrExited
	}
	if err != nil {
		return fmt.Errorf("cannot read when process %d started: %v", pid, err)
	}
	if _, err := st.Started(id, pid, start*1000, goneRuns(st.Load().Runs(), deps.Probe), now); err != nil {
		if errors.Is(err, state.ErrNotPlaced) {
			err = fmt.Errorf("%w (%s): a placement counts for %s and is then forgotten", err, id, placement.PendingFor)
		}
		return err
	}
	return nil
}

// goneRuns is the runs whose process is proven over, from one sample per pid:
// what a placement's own reading proves (retiredRuns), for a command that
// reads nothing else.
func goneRuns(runs []state.Run, probe sessions.PIDProbe) []state.Run {
	claims := make([]sessions.RegistryEntry, len(runs))
	for i, r := range runs {
		claims[i] = runEntry("", r)
	}
	evidence, _ := sessions.InspectClaims(claims, probe)
	return retiredRuns(runs, evidence)
}
