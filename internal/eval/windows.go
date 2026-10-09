package eval

// Every limit window the readings saw: the ones that reached the near-limit
// threshold, and the weekly ones that renewed.

import (
	"cmp"
	"slices"
)

// window is one limit of one account between two resets.
type window struct {
	key     string
	id      string
	label   string
	session bool
	reset   int64 // to the minute: the vendor's instant drifts by fractions of a second
	length  int64 // seconds; 0 = nothing states it
	period  int64
	start   int64
	points  []point
}

func (w *window) firstAt(threshold int) (int64, bool) {
	for _, p := range w.points {
		if p.percent >= threshold {
			return p.at, true
		}
	}
	return 0, false
}

func (w *window) last() point { return w.points[len(w.points)-1] }

func (w *window) peak() int {
	peak := 0
	for _, p := range w.points {
		peak = max(peak, p.percent)
	}
	return peak
}

func (e *evaluator) collectWindows() []*window {
	byID := map[string]*window{}
	var all []*window
	for _, k := range e.keyOrder {
		for _, r := range e.readings[k] {
			for _, w := range r.rows {
				if w.bad || w.reset <= 0 {
					continue
				}
				reset := minute(w.reset)
				id := k + "\x00" + w.id + "\x00" + stamp(reset)
				win, ok := byID[id]
				if !ok {
					win = &window{key: k, id: w.id, label: w.label, session: w.session, reset: reset, period: w.period,
						length: cmp.Or(w.window, w.period)}
					byID[id] = win
					all = append(all, win)
				}
				win.points = append(win.points, w.point)
			}
		}
	}
	slices.SortStableFunc(all, func(a, b *window) int {
		return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.id, b.id), cmp.Compare(a.reset, b.reset))
	})
	// A window starts its stated length before its reset. One whose length
	// nothing states — Claude Code's session window starts at the first
	// request after the last one ended — starts no earlier than the previous
	// window of the same limit reset, and no later than its first reading.
	for i, w := range all {
		switch {
		case w.length > 0:
			w.start = w.reset - w.length
		case i > 0 && all[i-1].key == w.key && all[i-1].id == w.id && all[i-1].reset < w.points[0].at:
			w.start = all[i-1].reset
		default:
			w.start = w.points[0].at
		}
	}
	return all
}

func (e *evaluator) windows(rep *Report) {
	all := e.collectWindows()
	e.starts = map[string]int64{}
	for _, w := range all {
		if w.session {
			e.starts[w.key+"\x00"+stamp(w.reset)] = w.start
		}
	}
	for _, w := range all {
		if ep, ok := e.episode(w); ok {
			rep.Episodes = append(rep.Episodes, ep)
		}
	}
	e.renewals(rep, all)
	slices.SortStableFunc(rep.Episodes, func(a, b Episode) int {
		return cmp.Or(cmp.Compare(a.NearAt, b.NearAt), cmp.Compare(a.Account, b.Account))
	})
	slices.SortStableFunc(rep.Renewals, func(a, b Renewal) int {
		return cmp.Or(cmp.Compare(a.RenewedAt, b.RenewedAt), cmp.Compare(a.Account, b.Account))
	})
}

// episode is a window that reached the near-limit threshold, with what was
// launched into it and the room another account had at that moment.
func (e *evaluator) episode(w *window) (Episode, bool) {
	nearAt, near := w.firstAt(NearPercent)
	if !near || nearAt < e.since {
		return Episode{}, false
	}
	by := e.placedBy(w.key, w.start, nearAt)
	s, rule := e.charged(nearAt, by)
	ep := Episode{
		Account: e.display(w.key), Key: w.key, Limit: w.label, Session: w.session, Rule: rule, Placed: by != "",
		NearAt: stamp(nearAt), Peak: w.peak(), ResetsAt: stamp(w.reset),
		Launches: map[string]int{}, ByHome: map[string]int{}, Elsewhere: e.roomAt(nearAt, w.key),
	}
	if at, ok := w.firstAt(FullPercent); ok {
		full := stamp(at)
		ep.ExhaustedAt = &full
	}
	for _, l := range e.launches {
		if l.chosenKey == w.key && l.at >= w.start && l.at <= nearAt {
			mode := l.Mode
			if l.automatic {
				mode = "auto"
			}
			ep.Launches[mode]++
			ep.ByHome[l.Home]++
		}
	}
	stats := &s.Session
	if !w.session {
		stats = &s.Weekly.LimitStats
	}
	stats.ReachedNear++
	if ep.ExhaustedAt != nil {
		stats.Exhausted++
	}
	if ep.Elsewhere != nil {
		stats.RoomElsewhere++
	}
	return ep, true
}

// renewals are the weekly windows read to their end. A renewal is the
// account's, not one row's: of the weekly limits that renew at one instant,
// the one read highest is what bound the account, and the room it left is what
// lapsed. A scoped limit nobody spends against renews at zero beside it, and
// its room could not have been spent without spending the binding limit's.
func (e *evaluator) renewals(rep *Report, all []*window) {
	type renewalID struct {
		key   string
		reset int64
	}
	binding := map[renewalID]*window{}
	scheduled := map[string][]int64{} // key → the scheduled renewals read
	period := map[string]int64{}
	for _, w := range all {
		if w.session {
			continue
		}
		id := renewalID{w.key, w.reset}
		if b, ok := binding[id]; !ok || w.last().percent > b.last().percent {
			binding[id] = w
		}
		if w.period > 0 {
			scheduled[w.key] = append(scheduled[w.key], w.reset)
			period[w.key] = w.period
		}
	}
	// Renewals a schedule implies between two that were read: weeks nobody
	// observed, neither spent nor lapsed as far as anyone knows.
	for k, rs := range scheduled {
		slices.Sort(rs)
		rs = slices.Compact(rs)
		p := period[k]
		for i := 1; i < len(rs); i++ {
			for n := int64(1); n < (rs[i]-rs[i-1]+p/2)/p; n++ {
				if at := rs[i-1] + n*p; at >= e.since && at <= e.now {
					// Nobody read the account that week, so nobody knows
					// what was placed there: the rule in force carries it.
					s, _ := e.charged(at, e.spanAt(at).rule)
					s.Weekly.Unobserved++
				}
			}
		}
	}
	used := map[*RuleSummary]float64{}
	for _, w := range binding {
		if w.reset > e.now || w.reset < e.since {
			continue
		}
		last := w.last()
		from := w.reset - cmp.Or(w.length, assumedWeek)
		by := e.placedBy(w.key, from, w.reset)
		s, rule := e.charged(w.reset, by)
		rn := Renewal{
			Account: e.display(w.key), Key: w.key, Limit: w.label, Rule: rule, Placed: by != "", RenewedAt: stamp(w.reset),
			Used: last.percent, LastReadS: w.reset - last.at, Lapsed: max(0, FullPercent-last.percent),
		}
		for _, o := range all {
			if o.key == w.key || o.id != w.id {
				continue
			}
			for _, p := range o.points {
				if p.at >= from && p.at <= w.reset {
					rn.PeakElsewhere = max(rn.PeakElsewhere, p.percent)
				}
			}
		}
		s.Weekly.Renewals++
		s.Weekly.LapsedPts += rn.Lapsed
		used[s] += float64(rn.Used)
		if rn.Lapsed > 0 && rn.PeakElsewhere >= NearPercent {
			s.Weekly.LapsedSqueeze++
		}
		rep.Renewals = append(rep.Renewals, rn)
	}
	for s, sum := range used {
		s.Weekly.MeanUsed = sum / float64(s.Weekly.Renewals)
	}
}
