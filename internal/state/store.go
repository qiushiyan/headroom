package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/atomicfile"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
)

const (
	// The two waits differ because the two failures cost opposite amounts. A
	// claim that cannot take the lock costs nothing — no request was
	// authorized, and the next round asks again. A completion that cannot take
	// it throws away a request already spent: a refusal's backoff, or the very
	// observation this store exists to keep. So a claim gives up quickly and a
	// completion waits, which it can afford because completions happen on the
	// fetch goroutine and never on a surface's draw loop.
	claimWait    = 2 * time.Second
	completeWait = 10 * time.Second
	lockPoll     = 20 * time.Millisecond

	// A placement waits less than a claim: it sits on the path of the command
	// typed most, and failing to take the lock costs only the record — the
	// launch still chooses, from what it could read, and starts.
	placeWait = time.Second

	// rehomeGrace keeps the sweep away from a re-home whose transcript does not
	// exist *yet*. A first turn's session id is known before the vendor has
	// written a line of it, so a record that young is not an orphan; without
	// the grace, the second of two launches a moment apart would sweep the
	// first's routing before its transcript appeared.
	rehomeGrace = 10 * time.Minute

	// runKeep bounds a headless session's record by age, whether or not its
	// process still runs: far longer than any session such a caller starts,
	// and short enough that a record whose process nobody has seen exit — the
	// process table was never asked — does not count for days.
	runKeep = 12 * time.Hour

	// retention bounds the ledger by age. Sweeping by absence instead would
	// need a complete account registry, which no caller can promise: an
	// account's key comes from a vendor file Claude Code rewrites constantly,
	// so a torn read moves it, and a sweep would delete the live cooldown and
	// stored answer of an account that is sitting right there. Age cannot be
	// wrong about identity, and a record nobody has touched in a month has
	// neither a live cooldown nor figures worth showing.
	retention = 30 * 24 * time.Hour
)

// Store is the handle to one accounts root's state.json. Every mutation is a
// locked read-modify-write of its own; nothing is cached between calls, and
// no method holds a lock across work this package does not own.
//
// A store is opened from a scope, so it knows its root, whose responses it
// holds and the spacing its deadlines respect. One store per accounts root is
// what lets a second vendor add files without re-keying the first's: nothing
// in Claude Code's state.json changes when Codex is fetched.
type Store struct {
	root    string
	vendor  config.Vendor
	spacing time.Duration
}

func Open(scope config.Scope) *Store {
	return &Store{root: filepath.Clean(scope.AccountsRoot), vendor: scope.Vendor, spacing: scope.RequestSpacing()}
}

// Vendor is whose responses this store holds — and therefore which parser
// reads a body that came out of it.
func (s *Store) Vendor() config.Vendor { return s.vendor }

// Load reads the document. No lock: writes land by atomic rename, so a reader
// sees one whole version or the previous one.
func (s *Store) Load() Snapshot {
	return Snapshot{d: read(s.root), vendor: s.vendor, spacing: s.spacing}
}

// ceiling is the furthest out a deadline written by this code can sit: the
// maximum cooldown, or the scope's spacing when that is longer. Every clamp
// uses it, so a spacing above CooldownMax is never shortened by one.
func ceiling(spacing time.Duration) time.Duration { return max(CooldownMax, spacing) }

// Decision is Claim's answer for one account. The zero value denies, so a
// caller that ignores the error cannot fetch on it.
type Decision struct {
	Key          Key
	Permit       bool
	Generation   int64     // pass back to Complete; identifies this attempt
	NextEligible time.Time // when to try again; zero means now

	// Degraded means the denial is headroom's own problem, not the endpoint's
	// budget. The store knows the difference — an unreadable ledger is not a
	// refusal — and a caller that cannot see it renders "live check deferred"
	// over a file nobody could read, which is a statement about a budget the
	// endpoint has said nothing about.
	Degraded bool
}

// Outcome is what became of a permitted request. It is deliberately about the
// *request*, not the account: none of these values says anything about whether
// the account is usable.
type Outcome int

const (
	// OutcomeStored: 200 whose body parsed. The body becomes this account's
	// newest observation.
	OutcomeStored Outcome = iota
	// OutcomeSpent: the request completed and proved the budget recovered, but
	// left nothing worth storing — a 200 whose body did not parse.
	OutcomeSpent
	// OutcomeRefused: 429. Lengthens the quiet period; says nothing else.
	OutcomeRefused
	// OutcomeFailed: transport error or some other non-200. No evidence about
	// the budget either way, so the claim's ordinary spacing stands.
	OutcomeFailed
)

// Claim decides eligibility and records the claim in one locked section, then
// returns the permits.
//
// This is the whole point of the package. Deciding eligibility in one place
// and writing the claim in another — which is what a separate Eligible() and
// NoteAttempt() amount to — leaves the window where two processes both read
// "eligible" and both fetch. Here the permit *is* the write: whoever loses the
// lock race reads the winner's claim and is denied.
//
// The claim reaching disk is what authorizes traffic, so a failure to lock,
// decode or write denies every account rather than falling through to "try
// anyway".
func (s *Store) Claim(keys []Key, now time.Time) ([]Decision, error) {
	out := make([]Decision, len(keys))
	for i, k := range keys {
		out[i] = Decision{Key: k, NextEligible: now.Add(ceiling(s.spacing))}
	}
	err := s.update(claimWait, func(d *doc) error {
		d.sweepStale(now)
		if d.badAccounts {
			// No readable ledger. "Eligible" is then a guess, and a live
			// cooldown is exactly when that guess is worst — so the unreadable
			// bytes are set aside (never dropped: `check` reports them) and
			// every account starts quiet. This self-heals in one cooldown
			// rather than bricking until someone deletes the file.
			d.setAside("accounts", &d.badAccounts)
			for i, k := range keys {
				r := d.accounts[k.ID()]
				r.Name = k.Name
				r.Request.NextEligibleMS = now.Add(ceiling(s.spacing)).UnixMilli()
				d.accounts[k.ID()] = r
				out[i] = Decision{Key: k, NextEligible: now.Add(ceiling(s.spacing)), Degraded: true}
			}
			return nil
		}
		for i, k := range keys {
			id := k.ID()
			r := d.accounts[id]
			next, stepped := d.deadline(id, now, ceiling(s.spacing))
			if stepped {
				// Written down, so the silence ends one ceiling from this claim
				// rather than receding with every later claim's clock.
				r.Request.NextEligibleMS = next
				d.accounts[id] = r
				d.dirty = true
			}
			if now.UnixMilli() < next {
				out[i] = Decision{Key: k, NextEligible: time.UnixMilli(next)}
				continue
			}
			r.Name = k.Name
			r.Request.Generation++
			r.Request.LastAttemptMS = now.UnixMilli()
			r.Request.NextEligibleMS = now.Add(s.spacing).UnixMilli()
			d.accounts[id] = r
			d.dirty = true
			out[i] = Decision{
				Key:          k,
				Permit:       true,
				Generation:   r.Request.Generation,
				NextEligible: time.UnixMilli(r.Request.NextEligibleMS),
			}
		}
		return nil
	})
	if err != nil {
		for i, k := range keys {
			out[i] = Decision{Key: k, NextEligible: now.Add(ceiling(s.spacing))}
		}
		return out, err
	}
	return out, nil
}

// Complete applies the outcome of a permitted request.
//
// The generation is what makes a slow request harmless. A fetch can outlive
// the claim that authorized it — the client allows ten seconds, another
// process can claim, fetch and record inside that — and without the check, the
// straggler's older body would overwrite the newer one and reset its cooldown.
// A generation mismatch is not an error; it is a race this design tolerates,
// and the correct response is to drop the late answer.
// It hands back when this account may next be asked, so a caller rendering a
// refusal can say "next attempt in 4m" without duplicating the backoff
// arithmetic this package owns.
func (s *Store) Complete(k Key, generation int64, outcome Outcome, body []byte, at time.Time) (time.Time, error) {
	var next time.Time
	err := s.update(completeWait, func(d *doc) error {
		if d.badAccounts {
			return ErrCorrupt
		}
		id := k.ID()
		r, ok := d.accounts[id]
		if !ok || r.Request.Generation != generation {
			if ok {
				next = time.UnixMilli(r.Request.NextEligibleMS)
			}
			return nil
		}
		switch outcome {
		case OutcomeStored, OutcomeSpent:
			// Any completed 200 proves this account's budget recovered,
			// whatever the body turned out to say. Withholding the strike
			// reset until the body parses would make a later refusal escalate
			// as though refusals had been consecutive.
			r.Request.Strikes = 0
			r.Request.NextEligibleMS = at.Add(s.spacing).UnixMilli()
			if outcome == OutcomeStored && len(body) > 0 && len(body) <= BodyLimit {
				b := make([]byte, len(body))
				copy(b, body)
				r.Usage = &Observation{FetchedAtMS: at.UnixMilli(), Body: b}
			}
		case OutcomeRefused:
			r.Request.Strikes++
			cooldown := CooldownBase << (r.Request.Strikes - 1)
			if cooldown > CooldownMax || cooldown <= 0 {
				cooldown = CooldownMax
			}
			// A refusal never buys an earlier retry than an ordinary attempt.
			cooldown = max(cooldown, s.spacing)
			r.Request.NextEligibleMS = at.Add(cooldown).UnixMilli()
		case OutcomeFailed:
			// A dead network says nothing about the budget; the claim's own
			// spacing already stands.
		}
		r.Request.LastAttemptMS = at.UnixMilli()
		d.accounts[id] = r
		d.dirty = true
		next = time.UnixMilli(r.Request.NextEligibleMS)
		return nil
	})
	return next, err
}

// sweepStale drops ledger records nothing has touched in a month, inside
// whatever locked pass is already running — the ledger is bounded by how many
// accounts this machine has ever logged into, so it needs no operation of its
// own and no caller has to remember to call one.
//
// A record with no attempt time is left alone, because Claim writes such
// records itself: a quiet period reserved without an attempt. The quarantine
// of an unreadable ledger starts every account it is asked about that way, and
// so does the bound written down for a clock-stepped checkpoint on an account
// with no record yet. Swept by its zero attempt time, that quiet period would
// end at the very next claim.
func (d *doc) sweepStale(now time.Time) {
	if d.badAccounts {
		return
	}
	cutoff := now.Add(-retention).UnixMilli()
	for id, r := range d.accounts {
		if r.Request.LastAttemptMS > 0 && r.Request.LastAttemptMS < cutoff {
			delete(d.accounts, id)
			d.dirty = true
		}
	}
}

// Enumerator lists the session ids that exist right now. It is invoked *inside*
// the lock, which is the whole reason it is a function and not a set: a
// snapshot taken by the caller before the lock cannot see a session another
// launch re-homed since, and garbage-collecting against it would delete that
// re-home. ok=false means the enumeration failed and no sweep may happen — a
// partial listing would sweep every record whose transcript it missed.
type Enumerator func() (ids map[string]bool, ok bool)

// Launch is one launch to place: who may take it, how the account is to be
// decided, and — when it is for a session — what to record about where that
// session now runs.
type Launch struct {
	Candidates []placement.Candidate
	Intent     placement.Intent
	PID        int // the launching process's own, which the exec keeps
	Now        time.Time

	// Session is the explicit id of the session this launch is for, "" when it
	// is for none. Its routing is recorded as a re-home when the launch puts it
	// anywhere but Intent.Owner; a launch that follows the owner writes none.
	Session string

	// Live lists the transcripts that exist, for sweeping re-homes whose
	// sessions are gone. It runs inside the lock, for Enumerator's reason, and
	// only when this launch writes a re-home.
	Live Enumerator

	// ID names a headless placement, whose caller starts the session itself
	// and reports its process to Started; PID is then 0 until it does.
	ID string

	// Gone are headless runs the caller has proven over — their process has
	// exited, or its pid now belongs to a process started later. They are
	// dropped inside the lock; a run recorded since the caller looked is not
	// among them, so it cannot be dropped by a reader that never saw it.
	Gone []Run
}

// Run is one headless session's process: a caller that starts its own
// session on a placement reports the process, which spends the placed
// subscription until it exits. Whether it still runs is the reader's to
// verify, exactly as for a vendor's registry claim: the pid is alive and its
// kernel start is StartedMS.
type Run struct {
	Placement string `json:"placement"`
	Key       string `json:"account"`
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	StartedMS int64  `json:"started_ms"`
}

// validRuns rejects a section that decodes around a record nothing could
// count: a process with no pid, start or subscription.
func validRuns(runs []Run) bool {
	for _, r := range runs {
		if r.PID <= 0 || r.StartedMS <= 0 || r.Key == "" {
			return false
		}
	}
	return true
}

// sweepRuns drops the runs proven over and the ones past runKeep, and sets
// aside a section that would not decode; whatever it changes dirties the
// document.
func (d *doc) sweepRuns(gone []Run, now time.Time) {
	d.setAside("runs", &d.badRuns)
	old := now.Add(-runKeep).UnixMilli()
	kept := d.runs[:0]
	for _, r := range d.runs {
		if r.StartedMS < old || slices.Contains(gone, r) {
			d.dirty = true
			continue
		}
		kept = append(kept, r)
	}
	d.runs = kept
}

// Placed is Place's answer. The decision is always there; with no error, a
// decision that chose an account is in the record the next placement reads.
// SessionErr says why the session's routing was not recorded when it should
// have been: an unreadable sessions section is never written over, here as
// everywhere, and the launch goes ahead.
type Placed struct {
	Decision   placement.Decision
	SessionErr error
}

// Place chooses the account for one launch and records the choice, in one
// locked section.
//
// This is Claim's shape applied to launches. Choosing in one place and
// recording in another leaves the window where two launches started together
// both read an account as empty and both go there; here whoever loses the lock
// race reads the winner's placement and counts it. The rule itself is
// placement.Choose — pure, imported, and called by this package, so no caller
// hands a decision function into the lock and the document is published to
// nobody.
//
// A launch and what it means for a session are one fact, so they are one
// write: the load the next launch counts and the re-home that routes the
// session's next turn land together or not at all.
//
// An error means nothing was recorded. That is never a reason to stop a
// launch — for an automatic launch every usable account is a correct answer
// — so the decision is then made from an unlocked read and nothing is recorded.
func (s *Store) Place(l Launch) (Placed, error) {
	var out Placed
	now := l.Now
	err := s.update(placeWait, func(d *doc) error {
		// Disposable, like the request ledger: an unreadable record of
		// launches is set aside and starts from nothing.
		d.setAside("placements", &d.badPlacements)
		if d.placements.Prune(now, retention) {
			d.dirty = true
		}
		d.sweepRuns(l.Gone, now)
		out.Decision = placement.Choose(l.Candidates, d.placements, l.Intent, now)
		chosen, ok := out.Decision.Find(out.Decision.Chosen)
		if out.Decision.Chosen == "" || !ok {
			return nil
		}
		if chosen.Ref == "" {
			// What a re-home is kept under: one recorded without it would
			// route to nothing, and silently.
			return fmt.Errorf("%s was offered with no ref, so it cannot be recorded", chosen.Name)
		}
		d.placements.Record(l.ID, chosen.Key, chosen.Name, l.PID, now)
		d.dirty = true
		if l.Session == "" || chosen.Name == l.Intent.Owner {
			return nil
		}
		if d.badSessions {
			out.SessionErr = ErrCorrupt
			return nil
		}
		if l.Live != nil {
			// The sweep runs before the write, so the record this very call
			// writes can never be swept by it.
			if exists, ok := l.Live(); ok {
				young := now.Add(-rehomeGrace).UnixMilli()
				for sid, rec := range d.sessions {
					if !exists[sid] && rec.AtMS < young {
						delete(d.sessions, sid)
					}
				}
			}
		}
		d.sessions[l.Session] = sessions.OwnerRec{Account: chosen.Ref, AtMS: now.UnixMilli()}
		return nil
	})
	if err != nil {
		return Placed{Decision: placement.Choose(l.Candidates, s.Load().Placements(), l.Intent, now)}, err
	}
	return out, nil
}

// ErrNotPlaced is Started's answer for an id no recent placement carries: it
// was never placed, or it has stopped counting and been pruned.
var ErrNotPlaced = errors.New("no recent placement has that id")

// Started records the process a headless caller started on its placement: the
// placement is given the process, so the two are counted once, and the run is
// recorded so the process counts as busy on that subscription until it exits.
// Gone are runs the caller has proven over, dropped in the same write. A
// second report for one placement replaces the first.
func (s *Store) Started(id string, pid int, startedMS int64, gone []Run, now time.Time) (Run, error) {
	var out Run
	err := s.update(claimWait, func(d *doc) error {
		if d.badPlacements {
			return ErrCorrupt
		}
		p, ok := d.placements.Attach(id, pid)
		if !ok {
			return ErrNotPlaced
		}
		d.sweepRuns(gone, now)
		out = Run{Placement: id, Key: p.Key, Name: p.Name, PID: pid, StartedMS: startedMS}
		d.runs = slices.DeleteFunc(d.runs, func(r Run) bool { return r.Placement == id })
		d.runs = append(d.runs, out)
		d.dirty = true
		return nil
	})
	return out, err
}

// update is the only writer of one document. It is unexported on purpose: an
// exported transaction taking a caller closure would publish the whole
// document (every invariant this package enforces would become a caller's to
// remember) and would make re-entry possible — flock is per open file
// description, so a nested update opens a second descriptor and blocks
// against itself forever.
func (s *Store) update(wait time.Duration, fn func(*doc) error) error {
	lock, err := acquire(s.root, wait)
	if err != nil {
		return err
	}
	defer release(lock)

	d := read(s.root)
	if err := writable(d); err != nil {
		return err
	}
	if err := fn(d); err != nil {
		return err
	}
	if !d.dirty {
		return nil
	}
	return commitDoc(d)
}

// writable is why a document must not be written, or nil.
func writable(d *doc) error {
	switch {
	case d.readOnly():
		return ErrReadOnly
	case d.unreadable:
		// Something is in that file and nobody could read it. Writing a fresh
		// document here is how re-homes get destroyed by an I/O blip.
		return ErrUnreadable
	}
	return nil
}

func acquire(root string, wait time.Duration) (*os.File, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath(root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrBusy
		}
		time.Sleep(lockPoll)
	}
}

func release(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// commitDoc writes the document atomically to the root it was read from.
func commitDoc(d *doc) error {
	data, err := d.marshal()
	if err != nil {
		return err
	}
	path := statePath(d.root)
	if d.corruptDoc {
		// Unreadable, but not this code's to destroy: whatever is in there is
		// the only copy, and someone may want to look at it.
		_ = os.Rename(path, path+".unreadable")
	}
	return atomicfile.Write(path, "state-*.json", data)
}
