package state

import (
	"maps"
	"strings"
	"time"
)

// What another machine said about this home's accounts is kept beside what
// this machine asked itself, in the peers section: one record per machine,
// replaced whole by each answer. A machine can only ask the usage endpoint
// about an account whose access token it keeps live, and only a session on
// that machine keeps it so; the machine driving an account is therefore the
// one whose reading of it is current, and the readings of the machines
// together are current for every account in use on any of them.
//
// Nothing here is a request to a vendor. A pull asks another machine's
// headroom what it already holds, so it spends no account's budget and is
// not the claim's to authorize: ClaimPull only keeps the surfaces of one
// machine from asking the same machine at once, and the pull itself runs
// outside the lock like any fetch.

const (
	// PeerRetry is how long a machine that did not answer is left alone: a
	// laptop asleep or off the network stays so for longer than one spacing,
	// and every ask of it costs the asking surface a connection timeout.
	PeerRetry = 5 * time.Minute

	// peerReadingsMax bounds one machine's answer: a reading per account it
	// holds, and no home has nearly this many.
	peerReadingsMax = 256
)

// peerRec is what this machine last heard from one other: when it last
// asked, when an answer last arrived, why the newest ask failed ("" when it
// was answered), and the answer — that machine's own newest reading of each
// account it holds, by the key the ledger uses.
type peerRec struct {
	AttemptMS int64                  `json:"attempt_ms"`
	PulledMS  int64                  `json:"pulled_ms,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Readings  map[string]Observation `json:"readings,omitempty"`
}

// PeerStatus is how the newest pull from one machine went, for `check` to
// say: when this machine last asked, when it last had an answer, and why the
// newest ask failed.
type PeerStatus struct {
	AttemptMS, PulledMS int64
	Error               string
}

// ClaimPull reports whether the surface asking may pull from peer now, and
// records the ask if so. A pull is due one spacing after the last ask, or
// PeerRetry after one that failed. Decided and recorded under the lock, so of
// the board, a detached refresh and the statusline's refresh running at once
// only one opens a connection.
func (s *Store) ClaimPull(peer string, now time.Time) (bool, error) {
	permit := false
	err := s.update(claimWait, func(d *doc) error {
		d.setAside(secPeers)
		d.sweepPeers(now)
		r := d.peers[peer]
		wait := s.spacing
		if r.Error != "" {
			wait = max(wait, PeerRetry)
		}
		// An ask stamped in the future is a clock that stepped back; it must
		// not hold every pull off until the clock catches up.
		if r.AttemptMS > 0 && r.AttemptMS <= now.Add(futureSkew).UnixMilli() &&
			now.UnixMilli() < r.AttemptMS+wait.Milliseconds() {
			return nil
		}
		r.AttemptMS = now.UnixMilli()
		d.peers[peer] = r
		d.dirty, permit = true, true
		return nil
	})
	return permit && err == nil, err
}

// RecordPull lands one pull's outcome: the answer, which replaces whatever
// the machine said before, or why there was none, which leaves the last
// answer standing. A failure annotates what is known and never erases it.
// Readings beyond the bounds a store keeps are dropped.
func (s *Store) RecordPull(peer string, readings map[string]Observation, failure string, at time.Time) error {
	return s.update(completeWait, func(d *doc) error {
		d.setAside(secPeers)
		r := d.peers[peer]
		if failure != "" {
			r.Error = failure
		} else {
			kept := make(map[string]Observation, min(len(readings), peerReadingsMax))
			for id, o := range readings {
				if len(kept) == peerReadingsMax {
					break
				}
				if strings.HasPrefix(id, "uuid:") && o.FetchedAtMS > 0 && len(o.Body) > 0 && len(o.Body) <= BodyLimit {
					kept[id] = o
				}
			}
			r.Error, r.PulledMS, r.Readings = "", at.UnixMilli(), kept
		}
		d.peers[peer] = r
		d.dirty = true
		return nil
	})
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
	if !ok || len(o.Body) == 0 || o.FetchedAtMS <= 0 || o.FetchedAtMS > now.Add(futureSkew).UnixMilli() {
		return Observation{}, false
	}
	return o, true
}

// PeerStatus is how the newest pull from peer went; false when this machine
// has never asked it.
func (s Snapshot) PeerStatus(peer string) (PeerStatus, bool) {
	r, ok := s.d.peers[peer]
	if !ok {
		return PeerStatus{}, false
	}
	return PeerStatus{AttemptMS: r.AttemptMS, PulledMS: r.PulledMS, Error: r.Error}, true
}

// sweepPeers drops the record of a machine nobody has asked in the ledger's
// retention: one taken out of HEADROOM_PEERS, whose readings no reader
// consults any more. Bounded by age, like the ledger, never by which machines
// the caller is configured with — a shell with another configuration is not
// evidence that a machine is gone.
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
