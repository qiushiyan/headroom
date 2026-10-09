package eval

// What the logs say, put in one timeline: the launches oldest first, and every
// account's readings oldest first.

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/usage"
)

// point is one reading of one limit.
type point struct {
	at      int64
	percent int
	bad     bool
	reset   int64 // as read; 0 = none
}

// row is one limit of one reading, as the rule counts limits: only the rows
// that bound ordinary work, with the session window marked.
type row struct {
	id      string // the limit across readings: see rowID
	label   string
	session bool
	period  int64
	window  int64
	point
}

type reading struct {
	at      int64
	key     string
	rows    []row
	blocked bool
}

type launch struct {
	Launch
	at        int64
	automatic bool
	chosenKey string
}

type eligibility struct {
	at int64
	ok bool
}

type evaluator struct {
	in       Input
	since    int64
	now      int64
	launches []launch             // oldest first
	readings map[string][]reading // by key, oldest first
	keyOrder []string             // the readings' keys, sorted: every walk over accounts is in one order
	names    map[string]string    // key → the name a person reads
	keys     map[string]string    // home + "\x00" + name → key
	eligible map[string][]eligibility
	sources  Sources

	// wins is every limit window the readings saw, by key and limit, in
	// reset order, each with the launches that went into it: the one
	// association decisions, episodes and renewals are all read from.
	wins map[string][]*window

	byRule   map[string]*RuleSummary
	unplaced *RuleSummary
}

func newEvaluator(in Input) *evaluator {
	e := &evaluator{
		in: in, now: in.Now.Unix(),
		readings: map[string][]reading{}, names: map[string]string{}, eligible: map[string][]eligibility{},
		byRule: map[string]*RuleSummary{}, unplaced: newSummary(""),
	}
	if !in.Since.IsZero() {
		e.since = in.Since.Unix()
	}
	e.sources.Skipped = in.Skipped
	e.sources.Problems = append([]string{}, in.Problems...)
	e.learnKeys()
	e.readLaunches()
	e.readUsage()
	e.collectWindows()
	return e
}

// learnKeys names the subscription behind each account name: the caller's
// map, from discovery, then whatever the logs themselves say — a usage line
// and a version 2 launch line each name a subscription beside the account's
// name in its home.
func (e *evaluator) learnKeys() {
	e.keys = maps.Clone(e.in.Keys)
	if e.keys == nil {
		e.keys = map[string]string{}
	}
	learn := func(home, name, key string) {
		if id := home + "\x00" + name; key != "" && name != "" && e.keys[id] == "" {
			e.keys[id] = key
		}
	}
	for _, r := range e.in.Usage {
		learn(r.Home, r.Name, r.Key)
	}
	for _, l := range e.in.Launches {
		for _, c := range l.Candidates {
			learn(l.Home, c.Name, c.Key)
		}
	}
}

func (e *evaluator) readLaunches() {
	homes := map[string]bool{}
	unkeyed := map[string]bool{}
	for _, l := range e.in.Launches {
		if l.Vendor != "" && l.Vendor != string(e.in.Vendor) {
			continue
		}
		t, err := time.Parse(time.RFC3339, l.At)
		if err != nil {
			continue
		}
		ll := launch{Launch: l, at: t.Unix(), automatic: automatic(l.Record)}
		for _, c := range l.Candidates {
			if e.keyOf(l.Home, c) == "" {
				unkeyed[l.Home+"\x00"+c.Name] = true
			}
			k := e.keyOrName(l.Home, c)
			if c.Name == l.Chosen {
				ll.chosenKey = k
			}
			e.name(k, c.Name, l.Home)
		}
		e.launches = append(e.launches, ll)
		homes[l.Home] = true
	}
	slices.SortStableFunc(e.launches, func(a, b launch) int { return cmp.Compare(a.at, b.at) })
	e.sources.Launches, e.sources.Homes, e.sources.Unkeyed = len(e.launches), len(homes), len(unkeyed)
}

// readUsage puts the usage log's readings on the timeline, then the figures
// launch lines carried that the log does not hold: the same reading is one
// reading, whichever file says it.
func (e *evaluator) readUsage() {
	vendor := e.in.Vendor
	seen := map[string]bool{}
	for _, r := range e.in.Usage {
		if r.Vendor != "" && r.Vendor != string(vendor) {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.At)
		if err != nil || seen[r.Key+"\x00"+r.At] {
			continue
		}
		seen[r.Key+"\x00"+r.At] = true
		rd := reading{at: t.Unix(), key: r.Key, blocked: r.Allowance == usage.AllowanceBlocked.Name()}
		rows := make([]usage.Row, len(r.Rows))
		for i, lr := range r.Rows {
			rows[i] = lr.Usage()
		}
		session := usage.SessionWindow(vendor, rows)
		for i, u := range rows {
			if !usage.General(vendor, u) {
				continue
			}
			rd.rows = append(rd.rows, row{
				id: rowID(u.Kind, u.Label, u.WindowSeconds), label: u.Label, session: i == session,
				period: int64(usage.Period(vendor, u) / time.Second), window: u.WindowSeconds,
				point: point{at: rd.at, percent: u.Percent, bad: u.PercentState == usage.StateBad, reset: u.ResetAt},
			})
		}
		e.readings[r.Key] = append(e.readings[r.Key], rd)
		e.name(r.Key, r.Name, r.Home)
		e.sources.Logged++
	}
	for _, l := range e.launches {
		for _, c := range l.Candidates {
			k := e.keyOrName(l.Home, c)
			e.eligible[k] = append(e.eligible[k], eligibility{at: l.at, ok: c.Eligible})
			if c.ObservedAt == nil {
				continue
			}
			t, err := time.Parse(time.RFC3339, *c.ObservedAt)
			if err != nil || seen[k+"\x00"+*c.ObservedAt] {
				continue
			}
			seen[k+"\x00"+*c.ObservedAt] = true
			rd := reading{at: t.Unix(), key: k, blocked: c.Excluded == "blocked by the vendor"}
			for _, lim := range c.Limits {
				rd.rows = append(rd.rows, row{
					id: rowID(lim.Kind, lim.Label, lim.WindowS), label: lim.Label, session: lim.Session,
					period: lim.PeriodS, window: lim.WindowS,
					point: point{at: rd.at, percent: lim.Percent, bad: lim.Basis == string(placement.BasisBad), reset: unix(lim.ResetsAt)},
				})
			}
			e.readings[k] = append(e.readings[k], rd)
			e.sources.Carried++
		}
	}
	first, last := int64(0), int64(0)
	note := func(at int64) {
		if first == 0 || at < first {
			first = at
		}
		last = max(last, at)
	}
	for k, rs := range e.readings {
		slices.SortStableFunc(rs, func(a, b reading) int { return cmp.Compare(a.at, b.at) })
		for _, r := range rs {
			note(r.at)
		}
		e.readings[k] = rs
		e.keyOrder = append(e.keyOrder, k)
	}
	slices.Sort(e.keyOrder)
	for _, l := range e.launches {
		note(l.at)
	}
	if first > 0 {
		f, l := stamp(first), stamp(last)
		e.sources.First, e.sources.Last = &f, &l
	}
}

// automatic reports that the rule chose. A version 1 line does not say; its
// mode does for a launch, and its reason for the picker, whose forced choices
// carry a reason the rule never gives.
func automatic(r launchlog.Record) bool {
	if r.V >= 2 {
		return r.Automatic
	}
	switch r.Mode {
	case "auto":
		return true
	case "picker":
		switch r.Reason {
		case placement.ReasonLeastLoad, placement.ReasonNearLimit, placement.ReasonMoved, placement.ReasonRotation:
			return true
		}
	}
	return false
}

// rowID names one limit across readings. A launch line carries no decoded
// group or model, so the label the parser derives from them stands in for
// them: it is compared whole, as an identity, and never matched as prose.
func rowID(kind, label string, window int64) string {
	return kind + "\x00" + label + "\x00" + strconv.FormatInt(window, 10)
}

func (e *evaluator) keyOf(home string, c launchlog.Candidate) string {
	if c.Key != "" {
		return c.Key
	}
	return e.keys[home+"\x00"+c.Name]
}

// keyOrName is the account's key, or — for an account no key is known for —
// an identity of its own, so it is still one account and never another's.
func (e *evaluator) keyOrName(home string, c launchlog.Candidate) string {
	if k := e.keyOf(home, c); k != "" {
		return k
	}
	return "name:" + c.Name + "@" + home
}

// name keeps the name a person reads for a key: the report's own home's, else
// whichever home's was seen last.
func (e *evaluator) name(key, name, home string) {
	if name == "" {
		return
	}
	if _, ok := e.names[key]; !ok || home == e.in.Home {
		e.names[key] = name
	}
}

func (e *evaluator) display(key string) string {
	if n, ok := e.names[key]; ok {
		return n
	}
	return key
}

// rule is one rule version's summary.
func (e *evaluator) rule(name string) *RuleSummary {
	s, ok := e.byRule[name]
	if !ok {
		s = newSummary(name)
		e.byRule[name] = s
	}
	return s
}

// charged is the summary a window counts under: the rule whose automatic
// launch went into it, or Unplaced.
func (e *evaluator) charged(by string) *RuleSummary {
	if by == "" {
		return e.unplaced
	}
	return e.rule(by)
}

// stateAt is an account's newest reading at or before a moment.
func (e *evaluator) stateAt(key string, at int64) (reading, bool) {
	rs := e.readings[key]
	i := sort.Search(len(rs), func(i int) bool { return rs[i].at > at })
	if i == 0 {
		return reading{}, false
	}
	return rs[i-1], true
}

// eligibleAt is whether the newest launch line before a moment, within a day,
// said the account could be chosen. No line says yes: missing bookkeeping
// excludes nothing, as with the rule.
func (e *evaluator) eligibleAt(key string, at int64) bool {
	es := e.eligible[key]
	i := sort.Search(len(es), func(i int) bool { return es[i].at > at })
	if i == 0 || at-es[i-1].at > int64(eligibleFor/time.Second) {
		return true
	}
	return es[i-1].ok
}

// counted is a reading as it stands at a moment, as the rule counts it: a row
// whose window has ended counts zero, and an old figure is a lower bound.
// ended says the session window has ended since the reading. ok is false
// when a figure did not parse.
func counted(r reading, at int64) (session, highest int, ended, ok bool) {
	for _, w := range r.rows {
		if w.bad {
			return 0, 0, false, false
		}
		v := w.percent
		if w.reset > 0 && w.reset <= at {
			v = 0
			ended = ended || w.session
		}
		if w.session {
			session = v
		}
		highest = max(highest, v)
	}
	return session, highest, ended, true
}

// roomOf is whether an account's newest reading shows room at a moment, and
// what says so. A fresh reading below the thresholds is room; an older one is
// a lower bound and shows room only where its session window has ended since
// — what nobody could ask is the idle account, and an ended window is empty
// unless it was used where nobody looked.
func (e *evaluator) roomOf(key string, at int64) (Room, bool) {
	if !e.eligibleAt(key, at) {
		return Room{}, false
	}
	r, ok := e.stateAt(key, at)
	if !ok || r.blocked {
		return Room{}, false
	}
	session, highest, ended, ok := counted(r, at)
	if !ok || highest >= NearPercent || session >= RoomBelow {
		return Room{}, false
	}
	room := Room{Account: e.display(key), Session: session, Highest: highest, AgeS: at - r.at}
	switch {
	case room.AgeS <= int64(FreshFor/time.Second):
		room.Basis = "fresh"
	case ended:
		room.Basis = "ended"
	default:
		return Room{}, false
	}
	return room, true
}

// roomAt is the other account with the most room at a moment, or nil.
func (e *evaluator) roomAt(at int64, except string) *Room {
	var best *Room
	for _, k := range e.keyOrder {
		if k == except {
			continue
		}
		room, ok := e.roomOf(k, at)
		if !ok {
			continue
		}
		if best == nil || cmp.Or(cmp.Compare(room.Session, best.Session), cmp.Compare(room.Highest, best.Highest), cmp.Compare(room.Account, best.Account)) < 0 {
			best = &room
		}
	}
	return best
}
