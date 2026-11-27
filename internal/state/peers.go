package state

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"
)

// What another machine said about this home's accounts is kept beside what
// this machine asked itself, in the peers section: one record per machine. A
// machine can only ask the usage endpoint about an account whose access token
// it keeps live, and only a session on that machine keeps it so; the machine
// driving an account is therefore the one whose reading of it is current, and
// the readings of the machines together are current for every account in use
// on any of them.
//
// Nothing here is a request to a vendor. A pull asks another machine's
// headroom what it already holds, so it spends no account's budget and is not
// the claim's to authorize: ClaimPull only keeps the surfaces of one machine
// from asking the same machine at once, and the pull itself runs outside the
// lock like any fetch.
//
// The section is read only by a store whose scope names peers. Under one that
// names none it is carried through writes as it stands, as any section this
// binary does not read: a machine on its own behaves as one that never knew
// of peers, leftovers included.

const (
	// PeerRetry is how long a machine that did not answer is left alone: a
	// laptop asleep or off the network stays so for longer than one spacing,
	// and every ask of it costs the asking surface a connection timeout.
	PeerRetry = 5 * time.Minute

	// peerReadingsMax bounds what is kept of one machine: a reading per
	// account it holds, and no home has nearly this many.
	peerReadingsMax = 256
)

// peerRec is what this machine last heard from one other. AttemptMS and
// Generation are the newest ask, Outcome the generation of the newest outcome
// recorded, so an ask newer than it has none yet. PulledMS is when an answer
// last landed; FailedMS when the newest outcome failed, Error why, both
// cleared by an answer — so which came last never rests on a clock, and a
// failure is never told under a later ask's time. Readings are the
// machine's own newest reading of each account it has answered for, by the
// ledger's key: an answer adds and renews them and never takes one away,
// since an answer that leaves an account out says nothing about the reading
// held, which stays a dated fact until a newer one replaces it or it ages
// out.
type peerRec struct {
	AttemptMS  int64                  `json:"attempt_ms"`
	Generation int64                  `json:"generation,omitempty"`
	Outcome    int64                  `json:"outcome_generation,omitempty"`
	PulledMS   int64                  `json:"pulled_ms,omitempty"`
	FailedMS   int64                  `json:"failed_ms,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Readings   map[string]Observation `json:"readings,omitempty"`
}

// failing reports that the newest outcome was a failure.
func (r peerRec) failing() bool { return r.FailedMS != 0 }

// nextPullMS is when a pull from this machine is next due, in Unix
// milliseconds; 0 means now. One spacing after the last ask, or PeerRetry
// after one that failed. An ask stamped in the future is a clock that stepped
// back, and must not hold every pull off until the clock catches up.
func (r peerRec) nextPullMS(spacing time.Duration, now time.Time) int64 {
	if r.AttemptMS <= 0 || r.AttemptMS > now.Add(futureSkew).UnixMilli() {
		return 0
	}
	wait := spacing
	if r.failing() {
		wait = max(wait, PeerRetry)
	}
	if next := r.AttemptMS + wait.Milliseconds(); next > now.UnixMilli() {
		return next
	}
	return 0
}

// PullClaim is ClaimPull's answer: whether to ask, and the generation the
// outcome must carry back. The zero value asks nothing.
type PullClaim struct {
	Permit     bool
	Generation int64
}

// PeerStatus is how pulling from one machine stands, for `check` to say:
// when it was last asked, whether that ask has an outcome yet, when an answer
// last landed, and — when the newest outcome failed — when and why.
type PeerStatus struct {
	AttemptMS, PulledMS, FailedMS int64
	Error                         string
	Pending                       bool
	Readings                      int
}

// ClaimPull says whether the surface asking may pull from peer now, and
// records the ask if so. Decided and recorded under the lock, so of the board,
// a detached refresh and the statusline's refresh running at once only one
// opens a connection. A store whose scope names no peer pulls from nobody.
func (s *Store) ClaimPull(peer string, now time.Time) (PullClaim, error) {
	var claim PullClaim
	if !s.paired {
		return claim, nil
	}
	err := s.update(claimWait, func(d *doc) error {
		d.setAside(secPeers)
		d.sweepPeers(now)
		r := d.peers[peer]
		if r.nextPullMS(s.spacing, now) > 0 {
			return nil
		}
		r.AttemptMS = now.UnixMilli()
		r.Generation++
		d.peers[peer] = r
		d.dirty = true
		claim = PullClaim{Permit: true, Generation: r.Generation}
		return nil
	})
	if err != nil {
		return PullClaim{}, err
	}
	return claim, nil
}

// RecordPull lands one pull's outcome: the answer's readings, each kept where
// it is newer than the one held, or why there was none. An outcome whose
// generation is not the newest ask's is a straggler that outlived its claim,
// and is dropped — a late answer must not stand over a newer one, nor a late
// timeout put a machine that has since answered back into retry. Readings
// beyond the bounds a store keeps are dropped.
func (s *Store) RecordPull(peer string, generation int64, readings map[string]Observation, failure string, at time.Time) error {
	return s.update(completeWait, func(d *doc) error {
		d.setAside(secPeers)
		r, ok := d.peers[peer]
		if !ok || r.Generation != generation {
			return nil
		}
		r.Outcome = generation
		if failure != "" {
			r.FailedMS, r.Error = at.UnixMilli(), failure
		} else {
			r.PulledMS, r.FailedMS, r.Error = at.UnixMilli(), 0, ""
			r.Readings = mergeReadings(r.Readings, readings, at)
		}
		d.peers[peer] = r
		d.dirty = true
		return nil
	})
}

// mergeReadings is held with fresh laid over it, newest per key, of the
// readings a reader would take (usableReading) — so one a reader refuses,
// stamped too far ahead, never stands over a usable one, and a record holding
// one recovers at the next answer — and the newest peerReadingsMax of them.
func mergeReadings(held, fresh map[string]Observation, at time.Time) map[string]Observation {
	out := make(map[string]Observation, len(held)+len(fresh))
	for _, m := range []map[string]Observation{held, fresh} {
		for id, o := range m {
			if !strings.HasPrefix(id, "uuid:") || !usableReading(o, at) || len(o.Body) > BodyLimit {
				continue
			}
			if prev, ok := out[id]; ok && prev.FetchedAtMS >= o.FetchedAtMS {
				continue
			}
			out[id] = o
		}
	}
	if len(out) > peerReadingsMax {
		ids := slices.SortedFunc(maps.Keys(out), func(a, b string) int { return cmp.Compare(out[b].FetchedAtMS, out[a].FetchedAtMS) })
		for _, id := range ids[peerReadingsMax:] {
			delete(out, id)
		}
	}
	return out
}

// PeerObservation is the reading peer last gave of this account, if one is
// usable. Only an account known by its identity is matched: a key that is a
// dir's name says nothing about which account another machine's dir of that
// name holds. A reading stamped in the future is refused as the store's own
// is (Observation).
func (s Snapshot) PeerObservation(peer string, k Key, now time.Time) (Observation, bool) {
	if k.UUID == "" || !s.d.has(secPeers) {
		return Observation{}, false
	}
	o, ok := s.d.peers[peer].Readings[k.ID()]
	if !ok || !usableReading(o, now) {
		return Observation{}, false
	}
	return o, true
}

// usableReading is a reading a reader takes at now: it has a body, and a time
// neither stamped in the future past the skew a clock may carry nor older
// than the ledger's retention.
func usableReading(o Observation, now time.Time) bool {
	return len(o.Body) > 0 && o.FetchedAtMS > now.Add(-retention).UnixMilli() && o.FetchedAtMS <= now.Add(futureSkew).UnixMilli()
}

// NextPull is when a pull from peer is next due, by the rule ClaimPull
// applies; the zero time means now. A surface deciding whether, or when, to
// run a round reads it, so a machine whose own accounts have nothing to ask
// still pulls; the claim alone authorizes. A document no claim can write — a
// newer headroom's, or one that cannot be read — says not soon, as
// NextEligible does, so nothing spins on a claim that must fail. A section
// that could not be read is set aside by the next pull, which is due.
func (s Snapshot) NextPull(peer string, now time.Time) time.Time {
	if s.d.readOnly() || s.d.cond == condUnreadable {
		return now.Add(ceiling(s.spacing))
	}
	if !s.d.has(secPeers) {
		return time.Time{}
	}
	if next := s.d.peers[peer].nextPullMS(s.spacing, now); next > 0 {
		return time.UnixMilli(next)
	}
	return time.Time{}
}

// PeerStatus is how pulling from peer stands; false when this machine has
// never asked it.
func (s Snapshot) PeerStatus(peer string) (PeerStatus, bool) {
	r, ok := s.d.peers[peer]
	if !ok || !s.d.has(secPeers) {
		return PeerStatus{}, false
	}
	return PeerStatus{AttemptMS: r.AttemptMS, PulledMS: r.PulledMS, FailedMS: r.FailedMS, Error: r.Error,
		Pending: r.Generation > r.Outcome, Readings: len(r.Readings)}, true
}

// sweepPeers drops the record of a machine nobody has asked in the ledger's
// retention: one taken out of HEADROOM_PEERS, whose readings no reader
// consults any more. Bounded by age, like the ledger, never by which machines
// the caller is configured with — a shell with another configuration is not
// evidence that a machine is gone. It runs at a pull; a home that names no
// peer at all carries the section as it stands.
func (d *doc) sweepPeers(now time.Time) {
	cutoff := now.Add(-retention).UnixMilli()
	for p, r := range d.peers {
		if r.AttemptMS < cutoff {
			delete(d.peers, p)
			d.dirty = true
		}
	}
}

// validPeerRecs reports that every record names its machine. headroom is the
// only writer, so a nameless one is damage rather than an old shape.
func validPeerRecs(m map[string]peerRec) bool {
	for p := range maps.Keys(m) {
		if p == "" {
			return false
		}
	}
	return true
}
