package app

// The launch operation: one decision, recorded, logged and announced,
// whichever surface asked for it. `headroom launch` and the session picker
// each build a launchRequest and hand it here with the facts gathered for it
// (launch_facts.go). What can be refused before anything is written is
// refused first (preflight), then `.current` is written if the launch was
// asked to remember; the store decides the account and records it, with what
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

	// remember records the decision in `.current` — the account forced, or
	// auto — before anything else is written. "Chosen and recorded" is one step
	// to the person, so a choice that cannot be recorded refuses the launch:
	// launching anyway would send the next bare launch somewhere else.
	remember bool

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

// preflight is every refusal a launch can know before it writes anything, and
// the one place each is decided: a dry run asks it, and so does placeLaunch
// before its first write. An account the launch was told to run on — by flag,
// by pin, or as a session's owner — that cannot be launched at all refuses
// with its own preparation error, said the way it always was. A launch that
// records auto as the mode refuses when the rule would choose nobody, since
// recording it would move every later bare launch on the strength of one that
// never ran.
//
// A forced refusal never reaches the store: Place registers the home, prunes
// and commits even when its rule refuses, and a launch that cannot start must
// leave nothing behind. What can still refuse afterwards is the store's own
// decision under its lock, and a re-home that must be written.
func preflight(req launchRequest, facts placeFacts) error {
	if req.intent.Kind == placement.Forced {
		if c, ok := facts.find(req.intent.Account); ok && c.prepareErr != nil {
			return c.prepareErr
		}
	}
	if req.remember && req.intent.Kind == placement.Auto {
		if d := placement.Choose(facts.rule(), facts.snap.Placements(), req.intent, req.now); d.Chosen == "" {
			return errors.New(d.Refusal)
		}
	}
	return nil
}

// placeLaunch refuses what preflight refuses, records `.current` when asked
// to, decides the account and records the launch and whatever it means for
// the session in one store operation, and appends the log line — in that
// order, so nothing is written for a launch that is then refused for a reason
// known before the write. An error is a refusal and nothing may start.
// Bookkeeping that merely failed is a note on the outcome, never a refusal: in
// auto mode every usable account is a correct answer.
//
// What a launch owes the next one is decided here too, not by the surface:
// a launch that writes a re-home sweeps the ones whose transcripts are gone,
// and a launch the rule placed leaves a refresh behind it. A surface that had
// to remember either would be a surface that forgot one. What stays with the
// surface is its terminal, its working directory and the exec.
func placeLaunch(req launchRequest, facts placeFacts) (launchOutcome, error) {
	out := launchOutcome{req: req}
	scope := facts.set.Scope
	if err := preflight(req, facts); err != nil {
		return out, err
	}
	if req.remember {
		if err := rememberChoice(req.intent, facts); err != nil {
			return out, fmt.Errorf("could not record .current (%v) — not launching", err)
		}
	}
	cands := facts.rule()
	launch := state.Launch{
		Candidates: cands, Intent: req.intent, PID: os.Getpid(), Now: req.now,
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
	// The rule chooses no account that cannot be launched — such a candidate
	// is excluded, and refused by name — so the chosen one was prepared.
	chosen, _ := facts.find(out.decision.Chosen)
	out.prepared, out.account = chosen.prepared, chosen.acct
	out.home, out.labels = facts.home, homeLabels(facts.snap)

	// RecordErr: the re-home this launch depended on is written, and the
	// shared ledger's write failed after it.
	missed := cmp.Or(placeErr, placed.RecordErr)
	if missed != nil {
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", missed))
	}
	if placed.SessionErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("session routing not recorded (%v) — run headroom check", placed.SessionErr))
	}
	rec := launchlog.New(out.decision, cands, placed.Ledger, req.intent, req.now)
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = string(scope.Vendor), os.Getpid(), req.cwd, req.mode, placed.Recorded
	rec.Session = req.ref.Route
	if missed != nil {
		rec.Problem = missed.Error()
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

// rememberChoice writes the decision to `.current`: auto, or the account the
// launch was told to run on.
func rememberChoice(intent placement.Intent, facts placeFacts) error {
	if intent.Kind == placement.Auto {
		return facts.set.SetAuto()
	}
	if c, ok := facts.find(intent.Account); ok {
		return facts.set.SetCurrent(c.acct)
	}
	return nil
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
