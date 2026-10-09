package eval

// Time on the Step grid: how long some account's session window sat near its
// limit, and how much of that time another account had room.

import (
	"cmp"
	"time"
)

func (e *evaluator) grid() {
	first, last := int64(0), int64(0)
	for _, rs := range e.readings {
		if len(rs) == 0 {
			continue
		}
		if first == 0 || rs[0].at < first {
			first = rs[0].at
		}
		last = max(last, rs[len(rs)-1].at)
	}
	if first == 0 {
		return
	}
	step := int64(Step / time.Second)
	fresh := int64(FreshFor / time.Second)
	start := max(first, e.since)
	start += (step - start%step) % step

	type state struct {
		key     string
		session int
		near    bool   // the session window, at or above NearPercent
		placed  string // ... and the rule whose automatic launch went into that window
		fresh   bool
		room    bool
	}
	spread := map[*RuleSummary][2]float64{} // sum, count
	for t := start; t <= min(last, e.now); t += step {
		var states []state
		anyFresh := false
		for _, k := range e.keyOrder {
			r, ok := e.stateAt(k, t)
			if !ok {
				continue
			}
			session, _, nearAny, ok := counted(r, t)
			if !ok {
				continue
			}
			// Near is the session window's: time a launch could have gone
			// elsewhere. An account set aside for a weekly figure near its
			// limit is the rule working, not load piling up.
			st := state{key: k, session: session, near: session >= NearPercent, fresh: t-r.at <= fresh}
			if st.near {
				st.placed = e.placedInSession(r, t)
			}
			st.room = !nearAny && !r.blocked && session < RoomBelow && e.eligibleAt(k, t)
			anyFresh = anyFresh || st.fresh
			states = append(states, st)
		}
		if !anyFresh {
			continue
		}
		hi, lo, nFresh := 0, FullPercent, 0
		near, squeezed, placed := false, false, ""
		for _, a := range states {
			if a.fresh {
				hi, lo, nFresh = max(hi, a.session), min(lo, a.session), nFresh+1
			}
			if !a.near {
				continue
			}
			near, placed = true, cmp.Or(placed, a.placed)
			for _, b := range states {
				squeezed = squeezed || (b.key != a.key && b.room)
			}
		}
		// A moment is the rule's while bare launches went automatic — and,
		// when a session window sat near its limit, only if the rule put work
		// into that window: one filled by pinned launches is not the rule's
		// doing, whatever the routing later became. The whole moment is
		// charged to one column, so no column's near time exceeds its
		// observed time.
		by := ""
		if sp := e.spanAt(t); sp.auto {
			by = sp.rule
		}
		if near {
			by = placed
		}
		s, _ := e.charged(t, by)
		s.Time.ObservedS += step
		if near {
			s.Time.NearS += step
		}
		if squeezed {
			s.Time.SqueezedS += step
		}
		if nFresh >= 2 {
			sp := spread[s]
			spread[s] = [2]float64{sp[0] + float64(hi-lo), sp[1] + 1}
		}
	}
	for s, sp := range spread {
		s.Time.MeanSpreadPts = round1(sp[0] / sp[1])
	}
}
