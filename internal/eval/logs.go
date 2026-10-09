package eval

// What the logs say, put in one timeline: the launches oldest first, every
// account's readings oldest first, and the rule and routing in force at any
// moment.

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

// span is the rule and routing in force from one of the report's home's
// launch lines on.
type span struct {
	at   int64
	rule string
	auto bool
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
	spans    []span               // oldest first
	readings map[string][]reading // by key, oldest first
	keyOrder []string             // the readings' keys, sorted: every walk over accounts is in one order
	names    map[string]string    // key → the name a person reads
	keys     map[string]string    // home + "\x00" + name → key
	eligible map[string][]eligibility
	sources  Sources

	byRule map[string]*RuleSummary
	pinned *RuleSummary

	// starts is when each session window began, by key and reset.
	starts map[string]int64
}

func newEvaluator(in Input) *evaluator {
	e := &evaluator{
		in: in, now: in.Now.Unix(),
		readings: map[string][]reading{}, names: map[string]string{}, eligible: map[string][]eligibility{},
		byRule: map[string]*RuleSummary{}, pinned: newSummary("pinned"),
	}
	if !in.Since.IsZero() {
		e.since = in.Since.Unix()
	}
	e.sources.Skipped = in.Skipped
	e.learnKeys()
	e.readLaunches()
	e.readUsage()
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

	// The rule and routing in force come from the report's own home: homes on
	// one machine can run binaries of different ages, what happened to the
	// accounts is the machine's, and the home a report is made from is the one
	// whose rule is being worked on. A home with no lines of its own takes
	// every home's. Routing is what a bare launch did: a line whose mode is
	// "auto" or "pinned" says it, and the others leave it as it was.
	own := slices.ContainsFunc(e.launches, func(l launch) bool { return l.Home == e.in.Home })
	auto := true
	for _, l := range e.launches {
		if l.Mode == "pinned" {
			auto = false
			break
		}
		if l.Mode == "auto" {
			break
		}
	}
	for _, l := range e.launches {
		if own && l.Home != e.in.Home {
			continue
		}
		switch l.Mode {
		case "auto":
			auto = true
		case "pinned":
			auto = false
		}
		e.spans = append(e.spans, span{at: l.at, rule: l.Rule, auto: auto})
	}
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

// rule is one rule version's summary. The empty rule — before the first
// launch line no rule is known to have been in force — gets a summary that is
// counted nowhere.
func (e *evaluator) rule(name string) *RuleSummary {
	if name == "" {
		return newSummary("")
	}
	s, ok := e.byRule[name]
	if !ok {
		s = newSummary(name)
		e.byRule[name] = s
	}
	return s
}

// spanAt is the rule and routing in force at a moment.
func (e *evaluator) spanAt(at int64) span {
	i := sort.Search(len(e.spans), func(i int) bool { return e.spans[i].at > at })
	if i == 0 {
		return span{}
	}
	return e.spans[i-1]
}

// charged is the summary something observed at a moment is charged to, and
// the rule it is said under. by is the rule whose automatic launch placed what
// was observed: the rule's own column when there was one, Pinned — under the
// rule in force — when there was none.
func (e *evaluator) charged(at int64, by string) (*RuleSummary, string) {
	if by != "" {
		return e.rule(by), by
	}
	sp := e.spanAt(at)
	if sp.rule == "" {
		return newSummary(""), ""
	}
	return e.pinned, sp.rule
}

// placedInSession is the rule of the newest automatic launch into the session
// window a reading describes, up to a moment; "" when none went there.
func (e *evaluator) placedInSession(r reading, at int64) string {
	for _, w := range r.rows {
		if w.session && w.reset > 0 {
			if start, ok := e.starts[r.key+"\x00"+stamp(minute(w.reset))]; ok {
				return e.placedBy(r.key, start, at)
			}
			return ""
		}
	}
	return ""
}

// placedBy is the rule of the newest automatic launch onto the account between
// two moments — whichever home made it — or "" when the rule placed nothing
// there.
func (e *evaluator) placedBy(key string, from, to int64) string {
	rule := ""
	i := sort.Search(len(e.launches), func(i int) bool { return e.launches[i].at >= from })
	for _, l := range e.launches[i:] {
		if l.at > to {
			break
		}
		if l.automatic && l.chosenKey == key {
			rule = l.Rule
		}
	}
	return rule
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
// whose window has ended counts zero, and an old figure is a lower bound. ok
// is false when a figure did not parse.
func counted(r reading, at int64) (session, highest int, near, ok bool) {
	for _, w := range r.rows {
		if w.bad {
			return 0, 0, false, false
		}
		v := w.percent
		if w.reset > 0 && w.reset <= at {
			v = 0
		}
		if w.session {
			session = v
		}
		highest = max(highest, v)
	}
	return session, highest, highest >= NearPercent, true
}

// roomAt is the other account with the most room at a moment, or nil. An
// account counts on its own newest figures, however old — an account nobody
// could ask is the idle one, and the rule tries it rather than avoiding it —
// and on the launch log's word that it could be chosen.
func (e *evaluator) roomAt(at int64, except string) *Room {
	var best *Room
	for _, k := range e.keyOrder {
		if k == except || !e.eligibleAt(k, at) {
			continue
		}
		r, ok := e.stateAt(k, at)
		if !ok || r.blocked {
			continue
		}
		session, highest, near, ok := counted(r, at)
		if !ok || near || session >= RoomBelow {
			continue
		}
		room := &Room{Account: e.display(k), Session: session, Highest: highest, AgeS: at - r.at}
		if best == nil || cmp.Or(cmp.Compare(room.Session, best.Session), cmp.Compare(room.Highest, best.Highest), cmp.Compare(room.Account, best.Account)) < 0 {
			best = room
		}
	}
	return best
}
