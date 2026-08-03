package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
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

	// retention bounds the ledger by age. Sweeping by absence instead would
	// need a complete account registry, which no caller can promise: an
	// account's key comes from a vendor file Claude Code rewrites constantly,
	// so a torn read moves it, and a sweep would delete the live cooldown and
	// stored answer of an account that is sitting right there. Age cannot be
	// wrong about identity, and a record nobody has touched in a month has
	// neither a live cooldown nor figures worth showing.
	retention = 30 * 24 * time.Hour
)

// Store is the handle to one accounts root's state file. Every mutation is a
// locked read-modify-write of its own; nothing is cached between calls, and no
// method holds the lock across work this package does not own.
type Store struct{ root string }

func Open(accountsRoot string) *Store { return &Store{root: accountsRoot} }

// Load reads the document. No lock: writes land by atomic rename, so a reader
// sees one whole version or the previous one.
func (s *Store) Load() Snapshot { return Snapshot{d: read(s.root)} }

// Decision is Claim's answer for one account. The zero value denies, so a
// caller that ignores the error cannot fetch on it.
type Decision struct {
	Key          Key
	Permit       bool
	Generation   int64     // pass back to Complete; identifies this attempt
	NextEligible time.Time // when to try again; zero means now
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
		out[i] = Decision{Key: k, NextEligible: now.Add(CooldownMax)}
	}
	err := s.update(claimWait, func(d *doc) error {
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
				r.Request.NextEligibleMS = now.Add(CooldownMax).UnixMilli()
				d.accounts[k.ID()] = r
				out[i] = Decision{Key: k, NextEligible: now.Add(CooldownMax)}
			}
			return nil
		}
		for i, k := range keys {
			id := k.ID()
			r := d.accounts[id]
			next := r.Request.NextEligibleMS
			// A legacy .throttle record is a floor, not a fallback: an upgrade
			// mid-cooldown must not look like a clean slate.
			if l, ok := d.legacy[k.Name]; ok {
				if l.NextEligibleMS > next {
					next = l.NextEligibleMS
				}
				if l.Strikes > r.Request.Strikes {
					r.Request.Strikes = l.Strikes
				}
			}
			if maxNext := now.Add(CooldownMax).UnixMilli(); next > maxNext {
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
			r.Request.NextEligibleMS = now.Add(spacing()).UnixMilli()
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
			out[i] = Decision{Key: k, NextEligible: now.Add(CooldownMax)}
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
			r.Request.NextEligibleMS = at.Add(spacing()).UnixMilli()
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
// process re-homed since, and garbage-collecting against it would delete that
// re-home. ok=false means the enumeration failed and no sweep may happen — a
// partial listing would sweep every record whose transcript it missed.
type Enumerator func() (ids map[string]bool, ok bool)

// ReHome records that the user explicitly routed a session to an account.
//
// The sweep runs before the mutation, so the record this very call writes can
// never be swept by it.
func (s *Store) ReHome(id, account string, at time.Time, live Enumerator) error {
	if id == "" || account == "" {
		return errors.New("state: re-home needs a session and an account")
	}
	return s.update(claimWait, func(d *doc) error {
		if d.badSessions {
			// Re-homes are user decisions. Writing a fresh section over bytes
			// that merely failed to decode would destroy them for good.
			return ErrCorrupt
		}
		if live != nil {
			if exists, ok := live(); ok {
				for sid := range d.sessions {
					if !exists[sid] {
						delete(d.sessions, sid)
					}
				}
			}
		}
		d.sessions[id] = OwnerRec{Account: account, AtMS: at.UnixMilli()}
		d.dirty = true
		return nil
	})
}

// Forget drops a session's re-home — a deleted transcript needs no routing
// preference. No sweep: this call knows exactly which record it means.
func (s *Store) Forget(id string) error {
	return s.update(claimWait, func(d *doc) error {
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

// update is the only writer. It is unexported on purpose: an exported
// transaction taking a caller closure would publish the whole document (every
// invariant this package enforces would become a caller's to remember) and
// would make re-entry possible — flock is per open file description, so a
// nested update opens a second descriptor and blocks against itself forever.
func (s *Store) update(wait time.Duration, fn func(*doc) error) error {
	lock, err := s.acquire(wait)
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	d := read(s.root)
	if d.readOnly() {
		return ErrReadOnly
	}
	if d.unreadable {
		// Something is in that file and nobody could read it. Writing a fresh
		// document here is how re-homes get destroyed by an I/O blip.
		return ErrUnreadable
	}
	if err := fn(d); err != nil {
		return err
	}
	if !d.dirty {
		return nil
	}
	return s.commit(d)
}

func (s *Store) acquire(wait time.Duration) (*os.File, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath(s.root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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

// commit writes the document atomically, then retires whatever legacy file it
// has finished absorbing.
func (s *Store) commit(d *doc) error {
	data, err := d.marshal()
	if err != nil {
		return err
	}
	path := statePath(s.root)
	if d.corruptDoc {
		// Unreadable, but not this code's to destroy: whatever is in there is
		// the only copy, and someone may want to look at it.
		_ = os.Rename(path, path+".unreadable")
	}
	tmp, err := os.CreateTemp(s.root, "state-*.json")
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
	s.retireLegacy(d)
	return nil
}

// retireLegacy removes a migrated file once it can no longer say anything the
// document does not.
//
// .owners goes as soon as its records are in the document; because the merge
// is newest-wins, an older binary still running that writes the file again
// simply gets absorbed by the next load.
//
// .throttle goes only once every deadline it carries has passed. Its entries
// are keyed by dir name while the ledger is keyed by account identity, so
// unlinking it early would drop the cooldowns of accounts this run never
// claimed — which is the upgrade-into-a-live-rate-limit this floor exists to
// prevent.
func (s *Store) retireLegacy(d *doc) {
	if len(d.sessions) > 0 || d.sessionsMigrated {
		_ = os.Remove(filepath.Join(s.root, ".owners"))
		_ = os.Remove(filepath.Join(s.root, ".owners.lock"))
	}
	if d.legacy == nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, l := range d.legacy {
		if l.NextEligibleMS > now {
			return
		}
	}
	_ = os.Remove(filepath.Join(s.root, ".throttle"))
}
