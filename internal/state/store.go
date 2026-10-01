package state

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

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

	// memberRefresh is how stale a home's registration may grow before a
	// request rewrites it. Registration is otherwise written only when it
	// changes, so a claim that permits nothing stays a read.
	memberRefresh = 24 * time.Hour

	// retention bounds the ledger by age. Sweeping by absence instead would
	// need a complete account registry, which no caller can promise: an
	// account's key comes from a vendor file Claude Code rewrites constantly,
	// so a torn read moves it, and a sweep would delete the live cooldown and
	// stored answer of an account that is sitting right there. Age cannot be
	// wrong about identity, and a record nobody has touched in a month has
	// neither a live cooldown nor figures worth showing.
	retention = 30 * 24 * time.Hour
)

// Store is the handle to one home's state: its own accounts root's file, and
// the subscription ledger — the same file, unless the home shares another
// home's ledger. Every mutation is a locked read-modify-write of its own;
// nothing is cached between calls, and no method holds a lock across work
// this package does not own.
//
// A store is opened from a scope, so it knows its roots, whose responses it
// holds and the spacing its deadlines respect. One store per accounts root is
// what lets a second vendor add files without re-keying the first's: nothing
// in Claude Code's state.json changes when Codex is fetched. A second home of
// the same vendor shares the first's ledger instead of keeping one, because
// the budget and the load are the subscription's, not the home's.
type Store struct {
	root    string // this home's accounts root: its re-homes, and its identity in the ledger
	ledger  string // the accounts root whose state.json holds the subscription ledger
	vendor  config.Vendor
	spacing time.Duration
	member  Member
}

func Open(scope config.Scope) *Store {
	root := filepath.Clean(scope.AccountsRoot)
	return &Store{
		root: root, ledger: filepath.Clean(scope.Ledger()), vendor: scope.Vendor, spacing: scope.RequestSpacing(),
		member: Member{Root: root, Home: scope.Home, Explicit: scope.PrimaryExplicit},
	}
}

// Vendor is whose responses this store holds — and therefore which parser
// reads a body that came out of it.
func (s *Store) Vendor() config.Vendor { return s.vendor }

// Home is this home's identity in the ledger: its accounts root, as every
// record of a launch or a busy session names it.
func (s *Store) Home() string { return s.root }

// Ledger is the accounts root whose state.json holds the subscription ledger.
func (s *Store) Ledger() string { return s.ledger }

func (s *Store) shared() bool { return s.ledger != s.root }

// Load reads the documents. No lock: writes land by atomic rename, so a reader
// sees one whole version or the previous one of each.
func (s *Store) Load() Snapshot {
	d := read(s.ledger)
	h := d
	if s.shared() {
		h = read(s.root)
	}
	return Snapshot{d: d, h: h, vendor: s.vendor, spacing: s.spacing}
}

// Register records this home in the ledger it spends against, so the other
// homes can find its account dirs. Every request and launch registers on its
// way; this is for a home that has just joined and done neither.
func (s *Store) Register(now time.Time) error {
	return s.update(s.ledger, claimWait, func(d *doc) error {
		d.register(s.member, now)
		return nil
	})
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
	err := s.update(s.ledger, claimWait, func(d *doc) error {
		d.register(s.member, now)
		d.sweepStale(now)
		if d.badAccounts {
			// No readable ledger. "Eligible" is then a guess, and a live
			// cooldown is exactly when that guess is worst — so the unreadable
			// bytes are set aside (never dropped: `check` reports them) and
			// every account starts quiet. This self-heals in one cooldown
			// rather than bricking until someone deletes the file.
			d.quarantine("accounts")
			d.badAccounts = false
			d.dirty = true
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
			next := r.Request.NextEligibleMS
			if d.imported != nil && d.imported.QuietUntilMS > next {
				next = d.imported.QuietUntilMS
			}
			if maxNext := now.Add(ceiling(s.spacing)).UnixMilli(); next > maxNext {
				// Further out than this code can produce: a clock step, not a
				// decision. Clamp rather than let it silence the account for
				// as long as the step.
				next = maxNext
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
	err := s.update(s.ledger, completeWait, func(d *doc) error {
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
// A record with no attempt time is left alone: this code always writes one, so
// its absence means a document shape this binary does not fully understand.
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

	// MustReHome says the launch exists to move the session — the picker's
	// override — so the re-home is written whoever the owner was, and a launch
	// whose re-home cannot be written is not recorded at all: the caller
	// refuses it, and nothing is left behind for the next launch to count.
	MustReHome bool

	// Live lists the transcripts that exist, for sweeping re-homes whose
	// sessions are gone. It runs inside the lock, for Enumerator's reason, and
	// only when this launch writes a re-home.
	Live Enumerator
}

// Placed is Place's answer. The decision is always there; the flags say what
// reached disk.
type Placed struct {
	Decision placement.Decision
	Recorded bool // the launch is in the record the next placement reads

	// RecordErr says why the launch is not in the record although Place
	// returned no error: the ledger is another home's file, the re-home this
	// launch depended on was written first, and the ledger's write then
	// failed. The launch goes ahead — it is routed — and misses only its load.
	RecordErr error

	// ReHomed reports that the session's routing was recorded. SessionErr says
	// why it was not when it should have been and the launch did not depend on
	// it: an unreadable sessions section is never written over, here as
	// everywhere.
	ReHomed    bool
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
// session's next turn land together or not at all. Recording them in two
// operations is how a resume that was then refused left a placement, a log
// line and a "last account" behind it.
//
// When the ledger is another home's file they are two documents, both locked
// for the whole operation — the ledger's lock first, always, so no two
// operations can wait on each other — and written in the order that keeps a
// refusal clean: the write a launch depends on goes first, and a failure there
// returns before anything else is written. A launch that must re-home writes
// the re-home first, so a refused move leaves nothing; any other launch writes
// its placement first, and a re-home that then fails is a note, exactly as an
// unreadable sessions section is.
//
// An error means nothing was recorded. For an ordinary launch that is never a
// reason to stop — in auto mode every usable account is a correct answer — so
// the decision is then made from an unlocked read and Recorded is false. For a
// launch that must re-home it is: the caller refuses.
func (s *Store) Place(l Launch) (Placed, error) {
	var out Placed
	now := l.Now
	l.Intent.Home = s.root
	err := s.updatePair(placeWait, func(led, home *doc, homeErr error) (commit, error) {
		if l.MustReHome && (homeErr != nil || home.badSessions) {
			// Re-homes are user decisions. Writing a fresh section over bytes
			// that merely failed to decode would destroy them for good.
			return commit{}, cmp.Or(homeErr, ErrCorrupt)
		}
		if led.badPlacements {
			// Disposable, like the request ledger: set the unreadable bytes
			// aside for `check` and start from nothing.
			led.quarantine("placements")
			led.badPlacements = false
			led.placements = placement.Ledger{}
			led.dirty = true
		}
		if led.placements.Prune(now, retention) {
			led.dirty = true
		}
		led.register(s.member, now)
		out.Decision = placement.Choose(l.Candidates, led.placements, l.Intent, now)
		chosen, ok := out.Decision.Find(out.Decision.Chosen)
		if out.Decision.Chosen == "" || !ok {
			return commit{ledger: true}, nil
		}
		led.placements.Record(chosen.Key, chosen.Name, l.PID, s.root, now)
		led.dirty = true
		out.Recorded = true
		if l.Session == "" || (!l.MustReHome && chosen.Name == l.Intent.Owner) {
			return commit{ledger: true}, nil
		}
		if homeErr != nil || home.badSessions {
			out.SessionErr = cmp.Or(homeErr, ErrCorrupt)
			return commit{ledger: true}, nil
		}
		if l.Live != nil {
			// The sweep runs before the write, so the record this very call
			// writes can never be swept by it.
			if exists, ok := l.Live(); ok {
				young := now.Add(-rehomeGrace).UnixMilli()
				for sid, rec := range home.sessions {
					if !exists[sid] && rec.AtMS < young {
						delete(home.sessions, sid)
					}
				}
			}
		}
		home.sessions[l.Session] = sessions.OwnerRec{Account: chosen.Name, AtMS: now.UnixMilli()}
		home.dirty = true
		out.ReHomed = true
		return commit{ledger: true, home: true, homeFirst: l.MustReHome}, nil
	})
	var partial pairErr
	switch {
	case errors.As(err, &partial) && partial.homeWritten:
		// The re-home this launch depended on is written; its load is not.
		out.Recorded, out.RecordErr = false, partial.err
		return out, nil
	case errors.As(err, &partial):
		// The placement is written; the session's routing is not.
		out.ReHomed, out.SessionErr = false, partial.err
		return out, nil
	case err != nil:
		out = Placed{Decision: placement.Choose(l.Candidates, s.Load().Placements(), l.Intent, now)}
		return out, err
	}
	return out, nil
}

// Forget drops a session's re-home — a deleted transcript needs no routing
// preference. No sweep: this call knows exactly which record it means.
func (s *Store) Forget(id string) error {
	return s.update(s.root, claimWait, func(d *doc) error {
		if d.badSessions {
			return ErrCorrupt
		}
		if _, ok := d.sessions[id]; !ok {
			return nil
		}
		delete(d.sessions, id)
		d.dirty = true
		return nil
	})
}

// update is the only writer of one document. It is unexported on purpose: an
// exported transaction taking a caller closure would publish the whole
// document (every invariant this package enforces would become a caller's to
// remember) and would make re-entry possible — flock is per open file
// description, so a nested update opens a second descriptor and blocks
// against itself forever.
func (s *Store) update(root string, wait time.Duration, fn func(*doc) error) error {
	lock, err := acquire(root, wait)
	if err != nil {
		return err
	}
	defer release(lock)

	d := read(root)
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
	case d.migrationErr != nil:
		return d.migrationErr
	}
	return nil
}

// commit says which of a pair's documents to write, and in which order.
type commit struct {
	ledger, home bool
	homeFirst    bool
}

// pairErr is a pair's second write failing after its first landed: half of
// the operation is on disk, and which half is the caller's to say.
type pairErr struct {
	err         error
	homeWritten bool // the home's document was the one that landed
}

func (e pairErr) Error() string { return e.err.Error() }

// updatePair is update over the ledger and this home's own document, for the
// one operation that writes both. When they are one file it is update. When
// they are two, the ledger's lock is taken first — the only operation holding
// two locks takes them in one order, so none can wait on another — and an
// unusable home document does not stop the ledger: fn is told why (homeErr)
// and decides what that costs.
func (s *Store) updatePair(wait time.Duration, fn func(led, home *doc, homeErr error) (commit, error)) error {
	if !s.shared() {
		return s.update(s.root, wait, func(d *doc) error {
			_, err := fn(d, d, nil)
			return err
		})
	}
	ll, err := acquire(s.ledger, wait)
	if err != nil {
		return err
	}
	defer release(ll)
	led := read(s.ledger)
	if err := writable(led); err != nil {
		return err
	}
	var home *doc
	hl, homeErr := acquire(s.root, wait)
	if homeErr == nil {
		defer release(hl)
		home = read(s.root)
		homeErr = writable(home)
	}
	if homeErr != nil {
		home = &doc{root: s.root, sessions: map[string]sessions.OwnerRec{}, badSessions: true}
	}
	c, err := fn(led, home, homeErr)
	if err != nil {
		return err
	}
	writeHome := c.home && home.dirty && homeErr == nil
	writeLedger := c.ledger && led.dirty
	if c.homeFirst {
		if writeHome {
			if err := commitDoc(home); err != nil {
				return err
			}
		}
		if writeLedger {
			if err := commitDoc(led); err != nil {
				if !writeHome {
					return err
				}
				return pairErr{err: err, homeWritten: true}
			}
		}
		return nil
	}
	if writeLedger {
		if err := commitDoc(led); err != nil {
			return err
		}
	}
	if writeHome {
		if err := commitDoc(home); err != nil {
			if !writeLedger {
				return err
			}
			return pairErr{err: err}
		}
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

// commitDoc writes the document atomically to the root it was read from, then
// retires whatever legacy file it has finished absorbing.
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
	tmp, err := os.CreateTemp(d.root, "state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync, not just rename: this document holds re-homes, and losing a human
	// decision to a crash is not the same class of loss as a forgotten
	// cooldown.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	archiveLegacy(d)
	return nil
}
