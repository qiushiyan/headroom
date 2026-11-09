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
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
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
	Mode   string // how the account came to be decided: auto, named, last

	// VendorArgs are a launch's pass-through arguments, for a launch that
	// names its session there. An automatic Claude Code launch reads them
	// (SessionOf): it follows the owner of a session named by id while that
	// account has room, and records the re-home when it places the session
	// elsewhere. A named launch was told where to go and reads nothing from
	// them; the arguments reach the vendor as given.
	VendorArgs []string

	CWD     string
	Environ []string // the environment a launch's child is built from
	Now     time.Time
}

// Plan is a request with the facts gathered for it, not yet decided.
type Plan struct {
	req   Request
	facts facts
	deps  Deps

	// What Gather derived about the session: the session the launch is for,
	// whose Record id is re-homed when the launch puts it somewhere other
	// than its owner, and the account that last drove it.
	session Session
	owner   string
}

// Gather reads what the request's placement needs from the disk: every
// account's figures and load, which of them an ordinary launch may run on,
// and how the launch stands to a session an automatic launch names in its
// arguments. Nothing is written.
func Gather(req Request, deps Deps) *Plan {
	p := &Plan{deps: deps}
	automatic := req.Intent.Kind == placement.Auto
	f := gather(req.Set, req.Store, readReservations(req.Set.Scope), req.Environ, req.CWD, automatic, req.Now, deps)
	if len(req.VendorArgs) > 0 && automatic && req.Set.Scope.Vendor == config.Claude {
		// A launch that names a session follows the account that last drove
		// it, so its prompt cache and its checkpoints stay usable. Only Claude
		// Code has an owner to ask about.
		p.session = SessionOf(req.VendorArgs)
		if p.session.Route != "" {
			p.owner = f.owner(p.session.Route)
			req.Intent.Owner = p.owner
		}
	}
	p.req, p.facts = req, f
	return p
}

// preflight is every refusal a launch can know before it writes anything, and
// the one place each is decided: a preview asks it, and so does Commit before
// its first write. An account the launch was told to run on — by name, or as
// a session's owner — that cannot be launched at all refuses with its own
// preparation error, said the way it always was.
//
// A forced refusal never reaches the store: Place registers the home, prunes
// and commits even when its rule refuses, and a launch that cannot start must
// leave nothing behind. What can still refuse afterwards is the store's own
// decision under its lock.
func (p *Plan) preflight() error {
	req, f := p.req, p.facts
	if req.Intent.Kind == placement.Auto && f.reservedErr != nil {
		return reservedRefusal(f.reservedErr)
	}
	if req.Intent.Kind == placement.Forced {
		if c, ok := f.find(req.Intent.Account); ok && c.prepareErr != nil {
			return c.prepareErr
		}
	}
	return nil
}

// reservedRefusal is why nothing automatic can be decided: which accounts are
// kept for what is routing state, so damage to it refuses, and never quietly
// releases an account.
func reservedRefusal(err error) error {
	return fmt.Errorf("%w — an automatic choice refuses until it is fixed, and a launch that names its account still runs", err)
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
	writeDryRun(w, binary, d, p.req.Mode, p.req.Now, p.facts)
	return d.Chosen != "", nil
}

// Commit refuses what Preview refuses, decides the account and records the
// launch and whatever it means for the session in one store operation, and
// appends the log line — in that order, so nothing is written for a launch
// that is then refused for a reason known before the write. An error is a
// refusal and nothing may start. Bookkeeping that merely failed is a note on
// the Launch, never a refusal: for an automatic launch every usable account
// is a correct answer.
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
	cands := f.rule()
	l := state.Launch{
		Candidates: cands, Intent: req.Intent, PID: os.Getpid(), Now: req.Now,
		Session: p.session.Record, Gone: f.gone,
	}
	if p.session.Record != "" {
		l.Live = func() (map[string]bool, bool) { return sessions.TranscriptIDs(scope.StoreDir()) }
	}
	placed, placeErr := req.Store.Place(l)
	out := &Launch{req: req, session: p.session, owner: p.owner, decision: placed.Decision}
	if out.decision.Chosen == "" {
		return nil, errors.New(out.decision.Refusal)
	}
	// The rule chooses no account that cannot be launched — such a candidate
	// is excluded, and refused by name — so the chosen one was prepared.
	chosen, _ := f.find(out.decision.Chosen)
	out.prepared, out.account = chosen.prepared, chosen.acct

	missed := placeErr
	if missed != nil {
		out.notes = append(out.notes, fmt.Sprintf("placement not recorded (%v) — the next launch will not count this one", missed))
	}
	if placed.SessionErr != nil {
		out.notes = append(out.notes, fmt.Sprintf("session routing not recorded (%v) — run headroom check", placed.SessionErr))
	}
	line := logLine{
		Vendor: string(scope.Vendor), Mode: req.Mode, Reason: out.decision.Reason, Chosen: out.decision.Chosen,
		CWD: req.CWD, Session: p.session.Route, PID: os.Getpid(),
	}
	if missed != nil {
		line.Problem = missed.Error()
	}
	if err := appendLog(scope.AccountsRoot, line, req.Now); err != nil {
		out.notes = append(out.notes, fmt.Sprintf("launch log not written (%v)", err))
	}
	if req.Intent.Kind == placement.Auto && worthRefreshing(f, req.Now) {
		// For the next launch, not this one: nothing here waits on a network.
		_ = p.deps.Refresh(scope)
	}
	return out, nil
}

// Launch is a launch that has been decided and recorded and has not yet
// replaced the process that asked for it.
type Launch struct {
	req      Request
	session  Session // the session it is for, as the plan derived it
	owner    string  // the account that last drove that session
	decision placement.Decision
	account  accounts.Account
	prepared launch.Prepared

	// notes are the bookkeeping that did not happen, worded for the person:
	// the launch proceeds regardless, and says so.
	notes []string
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
	fmt.Fprintln(w, prefix+launchLine(l.decision, l.req.Intent.Kind == placement.Auto, l.session, l.owner, l.req.Now))
	for _, note := range l.notes {
		fmt.Fprintln(w, prefix+note)
	}
}
