package app

// The launch operation: one decision, recorded, logged and announced,
// whichever surface asked for it. `headroom launch` and the session picker
// each build a launchRequest and hand it here with the facts gathered for it
// (launch_facts.go). The store decides the account and records it, with what
// the launch means for a session, in one locked section; the log line follows;
// and what a launch owes the next one — the refresh behind an automatic
// choice, the sweep of re-homes whose transcripts are gone — is decided here
// too. A surface keeps only its terminal, its working directory and the exec.

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

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
		Session: req.ref.Record, MustReHome: req.mustReHome, Gone: facts.gone,
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
	rec := launchlog.New(out.decision, facts.cands, placed.Ledger, req.intent, req.now)
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
