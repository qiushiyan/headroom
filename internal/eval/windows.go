package eval

// Every limit window the readings saw, and the launches that went into each:
// the one association the decisions, the episodes and the renewals are all
// read from.

import (
	"cmp"
	"slices"
	"time"
)

// window is one limit of one account between two resets.
type window struct {
	key      string
	id       string
	label    string
	session  bool
	reset    int64 // to the minute: the vendor's instant drifts by fractions of a second
	length   int64 // seconds; 0 = nothing states it
	period   int64
	start    int64
	points   []point  // oldest first
	launches []launch // the launches onto the account from start until the reset, oldest first
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

// placedBy is the rule of the newest automatic launch into the window up to a
// moment, whichever home made it; "" when the rule put nothing there.
func (w *window) placedBy(to int64) string {
	rule := ""
	for _, l := range w.launches {
		if l.at > to {
			break
		}
		if l.automatic {
			rule = l.Rule
		}
	}
	return rule
}

func (e *evaluator) collectWindows() {
	byID := map[string]*window{}
	e.wins = map[string][]*window{}
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
					e.wins[k+"\x00"+w.id] = append(e.wins[k+"\x00"+w.id], win)
				}
				win.points = append(win.points, w.point)
			}
		}
	}
	// A window starts its stated length before its reset. One whose length
	// nothing states starts no earlier than Follow before its reset, and no
	// earlier than the previous window of the same limit reset — Claude Code's
	// session window opens at the first request after the last one ended.
	for _, ws := range e.wins {
		slices.SortFunc(ws, func(a, b *window) int { return cmp.Compare(a.reset, b.reset) })
		for i, w := range ws {
			switch {
			case w.length > 0:
				w.start = w.reset - w.length
			case i > 0:
				w.start = max(ws[i-1].reset, w.reset-int64(Follow/time.Second))
			default:
				w.start = w.reset - int64(Follow/time.Second)
			}
			w.start = min(w.start, w.points[0].at)
		}
	}
	// A launch went into the window of each limit that was open at it, or —
	// when none was — the first to open after it.
	for _, l := range e.launches {
		for _, k := range e.limitsOf(l.chosenKey) {
			if w := e.windowAt(k, l.at); w != nil {
				w.launches = append(w.launches, l)
			}
		}
	}
}

// limitsOf is the window lists of one account, one per limit.
func (e *evaluator) limitsOf(key string) []string {
	var out []string
	for k, ws := range e.wins {
		if ws[0].key == key {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// windowAt is the window of one limit a launch at a moment went into.
func (e *evaluator) windowAt(limit string, at int64) *window {
	for _, w := range e.wins[limit] {
		if w.start <= at && at < w.reset {
			return w
		}
	}
	return nil
}

// sessionAt is the session window of an account a launch at a moment went
// into.
func (e *evaluator) sessionAt(key string, at int64) *window {
	for _, limit := range e.limitsOf(key) {
		if ws := e.wins[limit]; ws[0].session {
			if w := e.windowAt(limit, at); w != nil {
				return w
			}
		}
	}
	return nil
}

func (e *evaluator) windows(rep *Report) {
	var all []*window
	for _, ws := range e.wins {
		all = append(all, ws...)
	}
	slices.SortFunc(all, func(a, b *window) int {
		return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.id, b.id), cmp.Compare(a.reset, b.reset))
	})
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

// episode is a window that reached the near-limit threshold: what was launched
// into it, how long it sat there, and the room another account had meanwhile.
// A percent never falls within a window, so a window near its limit at one
// reading stays near it until the reset.
func (e *evaluator) episode(w *window) (Episode, bool) {
	nearAt, near := w.firstAt(NearPercent)
	if !near || nearAt < e.since {
		return Episode{}, false
	}
	by := w.placedBy(nearAt)
	ep := Episode{
		Account: e.display(w.key), Key: w.key, Limit: w.label, Session: w.session, Rule: by, Placed: by != "",
		NearAt: stamp(nearAt), Peak: w.peak(), ResetsAt: stamp(w.reset),
		Launches: map[string]int{}, ByHome: map[string]int{}, Elsewhere: e.roomAt(nearAt, w.key),
	}
	if at, ok := w.firstAt(FullPercent); ok {
		full := stamp(at)
		ep.ExhaustedAt = &full
	}
	for _, l := range w.launches {
		if l.at > nearAt {
			break
		}
		mode := l.Mode
		if l.automatic {
			mode = "auto"
		}
		ep.Launches[mode]++
		ep.ByHome[l.Home]++
	}
	end := min(w.reset, e.now)
	ep.NearS = max(0, end-nearAt)
	step := int64(Step / time.Second)
	for t := nearAt; t < end; t += step {
		if e.roomAt(t, w.key) != nil {
			ep.SqueezedS += min(step, end-t)
		}
	}
	s := e.charged(by)
	stats := &s.Session
	if !w.session {
		stats = &s.Weekly.LimitStats
	}
	stats.ReachedNear++
	stats.NearS += ep.NearS
	stats.SqueezedS += ep.SqueezedS
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
// may have lapsed. A scoped limit nobody spends against renews at zero beside
// it, and its room could not have been spent without spending the binding
// limit's.
func (e *evaluator) renewals(rep *Report, all []*window) {
	type renewalID struct {
		key   string
		reset int64
	}
	binding := map[renewalID]*window{}
	var order []renewalID
	for _, w := range all {
		if w.session {
			continue
		}
		id := renewalID{w.key, w.reset}
		b, ok := binding[id]
		if !ok {
			order = append(order, id)
		}
		if !ok || w.last().percent > b.last().percent {
			binding[id] = w
		}
	}
	used := map[*RuleSummary]float64{}
	for _, id := range order {
		w := binding[id]
		if w.reset > e.now || w.reset < e.since {
			continue
		}
		last := w.last()
		by := w.placedBy(w.reset)
		rn := Renewal{
			Account: e.display(w.key), Key: w.key, Limit: w.label, Rule: by, Placed: by != "", RenewedAt: stamp(w.reset),
			UsedMin: last.percent, LastReadS: w.reset - last.at, LapsedMax: max(0, FullPercent-last.percent),
		}
		from := w.reset - cmp.Or(w.length, assumedWeek)
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
		s := e.charged(by)
		s.Weekly.Renewals++
		s.Weekly.LapsedMaxPts += rn.LapsedMax
		used[s] += float64(rn.UsedMin)
		if rn.LapsedMax > 0 && rn.PeakElsewhere >= NearPercent {
			s.Weekly.LapsedWhileSqueezed++
		}
		rep.Renewals = append(rep.Renewals, rn)
	}
	for s, sum := range used {
		s.Weekly.MeanUsedMin = sum / float64(s.Weekly.Renewals)
	}
}

// unobserved counts the weekly renewals a window's schedule implies between
// two that were read: weeks nobody observed.
func (e *evaluator) unobserved() int {
	n := 0
	scheduled := map[string][]int64{}
	period := map[string]int64{}
	for _, ws := range e.wins {
		for _, w := range ws {
			if !w.session && w.period > 0 {
				scheduled[w.key] = append(scheduled[w.key], w.reset)
				period[w.key] = w.period
			}
		}
	}
	for k, rs := range scheduled {
		slices.Sort(rs)
		rs = slices.Compact(rs)
		p := period[k]
		for i := 1; i < len(rs); i++ {
			for j := int64(1); j < (rs[i]-rs[i-1]+p/2)/p; j++ {
				if at := rs[i-1] + j*p; at >= e.since && at <= e.now {
					n++
				}
			}
		}
	}
	return n
}
