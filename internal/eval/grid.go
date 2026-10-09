package eval

// Time across every account on the Step grid: how long some session window sat
// near its limit, how much of that another account had room, and how far
// apart the accounts' session figures were. It is context and owned by no
// rule — the logs say which launches went into a window, never which rule a
// moment belongs to; a window's own time is its episode's.

import "time"

func (e *evaluator) fleet() FleetStats {
	out := FleetStats{UnobservedRenewals: e.unobserved()}
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
		return out
	}
	step := int64(Step / time.Second)
	fresh := int64(FreshFor / time.Second)
	start := max(first, e.since)
	start += (step - start%step) % step
	var spread, spreadN float64
	// A reading describes its account for FreshFor after it was taken.
	for t := start; t <= min(last+fresh, e.now); t += step {
		hi, lo, nFresh := 0, FullPercent, 0
		var near []string
		for _, k := range e.keyOrder {
			r, ok := e.stateAt(k, t)
			if !ok {
				continue
			}
			session, _, _, ok := counted(r, t)
			if !ok {
				continue
			}
			if t-r.at <= fresh {
				hi, lo, nFresh = max(hi, session), min(lo, session), nFresh+1
			}
			// Near is the session window's: time a launch could have gone
			// elsewhere. An account set aside for a weekly figure near its
			// limit is the rule working, not load piling up.
			if session >= NearPercent {
				near = append(near, k)
			}
		}
		if nFresh == 0 {
			continue
		}
		out.ObservedS += step
		if len(near) > 0 {
			out.NearS += step
			for _, k := range near {
				if e.roomAt(t, k) != nil {
					out.SqueezedS += step
					break
				}
			}
		}
		if nFresh >= 2 {
			spread, spreadN = spread+float64(hi-lo), spreadN+1
		}
	}
	if spreadN > 0 {
		out.MeanSpreadPts = round1(spread / spreadN)
	}
	return out
}
