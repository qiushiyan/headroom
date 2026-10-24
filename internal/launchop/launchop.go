// Package launchop is the placement operation: one account decided for a
// session, recorded, logged and announced, whichever surface asked. A caller
// hands it a Request — the discovered set, the store, how the account is to be
// decided and the session the launch is for — and gets back a Plan gathered
// from the disk, which it can preview (what a launch would decide, through the
// refusals a launch would make) or commit. Gathering, eligibility, the owner
// an automatic launch follows and the words a launch says are its own, so no
// caller can pair one set with another snapshot or skip a refusal.
//
// What a surface keeps is its terminal, its working directory and the exec.
// The board asks for marks (mark.go); a headless caller, which starts the
// session itself, places through the same reading with an eligibility rule of
// its own (headless.go).
package launchop

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// Deps are the edges a placement reads through: a Keychain, a process table
// and a process of its own it leaves behind, each replaced in tests.
type Deps struct {
	// Creds reads one Claude Code account's stored credential; "" when it
	// cannot be read. Read only when the rule chooses.
	Creds func(accounts.Account) string
	// Probe answers when a pid started: the one probe behind every liveness
	// read of a placement.
	Probe sessions.PIDProbe
	// Refresh starts the detached refresh an automatic choice leaves behind,
	// for the next launch's figures, and returns at once.
	Refresh func(config.Scope) error
}

// Request is what a surface asks for: how the account is to be decided, and
// what the launch means for a session.
type Request struct {
	Set    accounts.Set
	Store  *state.Store
	Intent placement.Intent
	Mode   string // how the account came to be decided: auto, pinned, named, last, picker

	// Session is the session the launch is for. Its Record id is re-homed
	// when the launch puts it somewhere other than its owner; MustReHome says
	// the launch exists to move it, and is refused if that cannot be recorded.
	Session    Session
	MustReHome bool

	// Owner is the account that last drove the session, when the surface
	// knows it.
	Owner string

	// VendorArgs are a launch's pass-through arguments, for a launch that
	// names its session there rather than in Session. An automatic Claude Code
	// launch reads them (SessionOf): it follows the owner of a session named
	// by id while that account has room, and records the re-home when it
	// places the session elsewhere. A named or pinned launch was told where to
	// go and reads nothing from them; the arguments reach the vendor as given.
	VendorArgs []string

	// Remember records the decision in `.current` — the account forced, or
	// auto — before anything else is written. "Chosen and recorded" is one step
	// to the person, so a choice that cannot be recorded refuses the launch:
	// launching anyway would send the next bare launch somewhere else.
	Remember bool

	CWD     string
	Environ []string // the environment a launch's child is built from
	Now     time.Time
}

// Plan is a request with the facts gathered for it, not yet decided.
type Plan struct {
	req   Request
	facts facts
	deps  Deps
}

// Gather reads what the request's placement needs from the disk: every
// account's figures and load, which of them an ordinary launch may run on,
// and — for an automatic launch that follows a session — the session's
// owner (Request.VendorArgs). Nothing is written.
func Gather(req Request, deps Deps) *Plan {
	automatic := req.Intent.Kind == placement.Auto
	req.Intent.Home = req.Store.Home()
	f := gather(req.Set, req.Store, req.Environ, automatic, req.Now, deps)
	if len(req.VendorArgs) > 0 && automatic && req.Set.Scope.Vendor == config.Claude {
		// A launch that names a session follows the account that last drove
		// it, so its prompt cache and its checkpoints stay usable. Only Claude
		// Code has an owner to ask about.
		req.Session = SessionOf(req.VendorArgs)
		if req.Session.Route != "" {
			req.Owner = f.owner(req.Session.Route)
			req.Intent.Owner = req.Owner
		}
	}
	return &Plan{req: req, facts: f, deps: deps}
}

// preflight is every refusal a launch can know before it writes anything, and
// the one place each is decided: a preview asks it, and so does Commit before
// its first write. An account the launch was told to run on — by flag, by pin,
// or as a session's owner — that cannot be launched at all refuses with its
// own preparation error, said the way it always was. A launch that records
// auto as the mode refuses when the rule would choose nobody, since recording
// it would move every later bare launch on the strength of one that never ran.
//
// A forced refusal never reaches the store: Place registers the home, prunes
// and commits even when its rule refuses, and a launch that cannot start must
// leave nothing behind. What can still refuse afterwards is the store's own
// decision under its lock, and a re-home that must be written.
func (p *Plan) preflight() error {
	req, f := p.req, p.facts
	if req.Intent.Kind == placement.Forced {
		if c, ok := f.find(req.Intent.Account); ok && c.prepareErr != nil {
			return c.prepareErr
		}
	}
	if req.Remember && req.Intent.Kind == placement.Auto {
		if d := placement.Choose(f.rule(), f.snap.Placements(), req.Intent, req.Now); d.Chosen == "" {
			return errors.New(d.Refusal)
		}
	}
	return nil
}

// Preview writes what a launch would decide — the whole table it would choose
// from — after the refusals it would make before writing anything. It records,
// logs and starts nothing. chosen is false when the rule would choose nobody;
// an error is a refusal the launch would make first.
func (p *Plan) Preview(w io.Writer, binary string) (chosen bool, err error) {
	if err := p.preflight(); err != nil {
		return false, err
	}
	d := placement.Choose(p.facts.rule(), p.facts.snap.Placements(), p.req.Intent, p.req.Now)
	writeDryRun(w, binary, d, p.req.Mode, p.req.Now, p.facts.home, p.facts.labels)
	return d.Chosen != "", nil
}

// Commit refuses what Preview refuses, records `.current` when asked to,
// decides the account and records the launch and whatever it means for the
// session in one store operation, and appends the log line — in that order,
// so nothing is written for a launch that is then refused for a reason known
// before the write. An error is a refusal and nothing may start. Bookkeeping
// that merely failed is a note on the Launch, never a refusal: in auto mode
// every usable account is a correct answer.
//
// What a launch owes the next one is decided here too, not by the surface: a
// launch that writes a re-home sweeps the ones whose transcripts are gone,
// and a launch the rule placed leaves a refresh behind it.
func (p *Plan) Commit() (*Launch, error) {
	req, f := p.req, p.facts
	scope := f.set.Scope
	if err := p.preflight(); err != nil {
		return nil, err
	}
	if req.Remember {
		if err := p.remember(); err != nil {
			return nil, fmt.Errorf("could not record .current (%v) — not launching", err)
		}
	}
	cands := f.rule()
	l := state.Launch{
		Candidates: cands, Intent: req.Intent, PID: os.Getpid(), Now: req.Now,
		Session: req.Session.Record, MustReHome: req.MustReHome, Gone: f.gone,
	}
	if req.Session.Record != "" {
		l.Live = func() (map[string]bool, bool) { return sessions.TranscriptIDs(scope.StoreDir()) }
	}
	placed, placeErr := req.Store.Place(l)
	if req.MustReHome && placeErr != nil {
		return nil, fmt.Errorf("re-home not recorded (%v) — enter resumes without it", placeErr)
	}
	out := &Launch{req: req, decision: placed.Decision, home: f.home, labels: f.labels}
	if out.decision.Chosen == "" {
		return nil, errors.New(out.decision.Refusal)
	}
	// The rule chooses no account that cannot be launched — such a candidate
	// is excluded, and refused by name — so the chosen one was prepared.
	chosen, _ := f.find(out.decision.Chosen)
	out.prepared, out.account = chosen.prepared, chosen.acct

	// RecordErr: the re-home this launch depended on is written, and the
	// shared ledger's write failed after it.
	missed := cmp.Or(placeErr, placed.RecordErr)
	if missed != nil {
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", missed))
	}
	if placed.SessionErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("session routing not recorded (%v) — run headroom check", placed.SessionErr))
	}
	rec := launchlog.New(out.decision, cands, placed.Ledger, req.Intent, req.Now)
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = string(scope.Vendor), os.Getpid(), req.CWD, req.Mode, placed.Recorded
	rec.Session = req.Session.Route
	if missed != nil {
		rec.Problem = missed.Error()
	}
	if err := launchlog.Append(scope.AccountsRoot, rec); err != nil {
		out.notes = append(out.notes, fmt.Sprintf("launch log not written (%v)", err))
	}
	if req.Intent.Kind == placement.Auto && worthRefreshing(f, req.Now) {
		// For the next launch, not this one: nothing here waits on a network.
		_ = p.deps.Refresh(scope)
	}
	return out, nil
}

// remember writes the decision to `.current`: auto, or the account the
// launch was told to run on.
func (p *Plan) remember() error {
	if p.req.Intent.Kind == placement.Auto {
		return p.facts.set.SetAuto()
	}
	if c, ok := p.facts.find(p.req.Intent.Account); ok {
		return p.facts.set.SetCurrent(c.acct)
	}
	return nil
}

// Launch is a launch that has been decided and recorded and has not yet
// replaced the process that asked for it.
type Launch struct {
	req      Request
	decision placement.Decision
	account  accounts.Account
	prepared launch.Prepared

	// notes are the bookkeeping that did not happen, worded for the person:
	// the launch proceeds regardless, and says so.
	notes []string

	// home and labels say whose load is whose: this home's accounts root,
	// and every home's name.
	home   string
	labels state.HomeLabels
}

// Account is the account the launch was placed on.
func (l *Launch) Account() accounts.Account { return l.account }

// Prepared is the vendor process the launch runs: its resolved path, argv[0]
// and environment, built from the decision alone.
func (l *Launch) Prepared() launch.Prepared { return l.prepared }

// Announce says, before the vendor starts, where the launch is going and why,
// and what bookkeeping was missed on the way. Every launch says it, whichever
// surface started it and however the account was decided: a line that appears
// for some launches and not others is one a person stops reading.
func (l *Launch) Announce(w io.Writer, prefix string) {
	for _, notice := range l.prepared.Notices {
		fmt.Fprintln(w, prefix+notice)
	}
	c, _ := l.decision.Find(l.decision.Chosen)
	fmt.Fprintln(w, prefix+launchLine(l.decision, l.req.Intent.Kind == placement.Auto, l.req.Session, l.req.Owner, elsewhere(c, l.home, l.labels), l.req.Now))
	for _, note := range l.notes {
		fmt.Fprintln(w, prefix+note)
	}
}
