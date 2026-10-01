// Package placement chooses the account a launch runs on. It is one pure
// function over recorded inputs — no clock, file or network of its own — so
// the rule can be tested by table and replaced without touching what gathers
// its inputs or what records its answer.
//
// The rule gives each limit one job. The session window (the vendor's shortest
// limit, five hours for Claude Code) ranks accounts, as load: its usage in
// steps of ten points, plus one step for each busy session and each launch too
// recent to show in the figures. Any limit at or above the near-limit
// threshold sets an account aside. Weekly room breaks ties. A rule that folds
// both windows into one figure lets the larger weekly number absorb every new
// launch, and launches then pile onto one account — the failure this exists to
// end.
//
// A figure is counted only for the window it describes. A row whose reset has
// passed counts as zero, a stale observation's rows are lower bounds, and an
// account that cannot be asked is tried rather than avoided: the idle accounts
// are the ones that cannot be asked. Every counted figure carries its basis,
// so a lower bound is never recorded as measured room.
package placement

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The constants are policy, chosen and not measured. They are named in Rule,
// so a log line says which values decided it.
const (
	Rule = "load-1"

	// StepPoints is one step of load: ten points of session-window usage, one
	// busy session, or one pending placement.
	StepPoints = 10

	// NearLimitPercent sets an account aside while another has room.
	NearLimitPercent = 80

	// StaleAfter is the age past which an observation's rows are lower bounds.
	StaleAfter = 15 * time.Minute

	// PendingFor is how long a launch counts as load on its account. It is a
	// fixed span and not "until the next observation": an observation taken a
	// second after a launch reflects none of what that session will spend, and
	// a short job that has already exited still spent what the figures have
	// not caught up with.
	PendingFor = 15 * time.Minute

	// dedupeSlackMS is how much earlier than its own placement a busy session's
	// registered start may be and still be the same process.
	dedupeSlackMS = 10_000
)

// Basis says what a counted figure rests on.
type Basis string

const (
	BasisObserved Basis = "observed" // a current observation's percent
	BasisStale    Basis = "stale"    // an old observation's percent: a lower bound
	BasisEnded    Basis = "ended"    // the window this row described has ended
	BasisBad      Basis = "bad"      // the percent did not parse; nothing bounds it
)

// Limit is one limit row of an account's newest observation.
type Limit struct {
	Kind    string // the vendor's word for the row
	Label   string // what a person reads
	Percent int
	Bad     bool  // the percent did not parse
	ResetAt int64 // unix seconds; 0 = none
	Session bool  // the session window
}

// Proc identifies a process: pids recycle, so the start instant travels along.
// Home is the accounts root of the home whose registry reported it — every
// home on the machine shares one pid space, so a launch recorded by one home
// and the session another home's registry shows are the same process.
type Proc struct {
	PID       int
	StartedMS int64
	Home      string
}

// Candidate is everything the rule knows about one account.
type Candidate struct {
	Name string // the account's name: what a launch selects and a line prints
	Key  string // the quota it spends; two dirs on one account share a key

	// Excluded says why an automatic choice avoids this account ("" = it may
	// be chosen). Unlaunchable narrows that: no launch can run here at all, so
	// a forced choice refuses too.
	Excluded     string
	Unlaunchable bool

	ObservedAt int64  // unix seconds; 0 = never observed
	Source     string // where the observation came from; carried for the record
	Limits     []Limit

	// Busy is every verified-live session the vendor reports as working on
	// this subscription, in any home that shares the ledger: two homes'
	// logins of one account spend one quota.
	Busy     []Proc
	Statuses []string // every verified-live session's status, as read
}

// Pending is one recorded launch. Home is the accounts root of the home that
// made it: the rule counts every home's alike, and a surface says whose.
type Pending struct {
	Key  string `json:"account"`
	Name string `json:"name"`
	PID  int    `json:"pid"`
	AtMS int64  `json:"at_ms"`
	Home string `json:"home,omitempty"`
}

// Last is the newest launch recorded on one account.
type Last struct {
	Name string `json:"name"`
	AtMS int64  `json:"at_ms"`
}

// Ledger is the record of launches the rule reads and the store keeps: the
// recent ones, which are load; the newest per subscription, which breaks
// ties; and the newest per home, which answers "the last account used".
//
// The last two are kept apart because their units differ. Ties are broken by
// how recently a subscription was placed, whichever home placed it. But an
// account name belongs to one home — two homes holding logins of one
// subscription name their dirs alike — so a launch in one home must never
// answer the other's --last.
type Ledger struct {
	Recent []Pending       `json:"recent,omitempty"`
	Last   map[string]Last `json:"last,omitempty"`
	ByHome map[string]Last `json:"by_home,omitempty"`
}

// Record adds a launch on an account, made by the home at that accounts root.
func (l *Ledger) Record(key, name string, pid int, home string, now time.Time) {
	at := now.UnixMilli()
	l.Recent = append(l.Recent, Pending{Key: key, Name: name, PID: pid, AtMS: at, Home: home})
	if l.Last == nil {
		l.Last = map[string]Last{}
	}
	if l.ByHome == nil {
		l.ByHome = map[string]Last{}
	}
	// Newest wins: two homes' clocks are one machine's, but a record is never
	// replaced by an older one.
	if cur, ok := l.Last[key]; !ok || at >= cur.AtMS {
		l.Last[key] = Last{Name: name, AtMS: at}
	}
	if cur, ok := l.ByHome[home]; !ok || at >= cur.AtMS {
		l.ByHome[home] = Last{Name: name, AtMS: at}
	}
}

// Prune drops what no longer counts: recent launches past PendingFor, and
// per-account records older than keep. It reports whether anything changed.
func (l *Ledger) Prune(now time.Time, keep time.Duration) bool {
	changed := false
	cutoff := now.Add(-PendingFor).UnixMilli()
	kept := l.Recent[:0]
	for _, p := range l.Recent {
		if p.AtMS >= cutoff {
			kept = append(kept, p)
		} else {
			changed = true
		}
	}
	l.Recent = kept
	old := now.Add(-keep).UnixMilli()
	for _, m := range []map[string]Last{l.Last, l.ByHome} {
		for k, rec := range m {
			if rec.AtMS < old {
				delete(m, k)
				changed = true
			}
		}
	}
	return changed
}

// Newest is the account of the newest launch the home at that accounts root
// recorded.
func (l Ledger) Newest(home string) (Last, bool) {
	rec, ok := l.ByHome[home]
	return rec, ok && rec.Name != ""
}

// IntentKind is how the account is to be decided.
type IntentKind int

const (
	Auto     IntentKind = iota // the rule chooses
	Forced                     // the caller names the account; the rule records it
	LastUsed                   // the account of the newest recorded launch
)

// Intent is what a launch asks of the rule.
type Intent struct {
	Kind    IntentKind
	Account string // Forced: the account
	Reason  string // Forced: why, for the record ("pinned", "named")

	// Home is the accounts root of the home placing the launch: whose last
	// launch LastUsed means, and whose share of the load is its own.
	Home string

	// Owner is the account that last drove the session this launch names, ""
	// when it names none or none is known. Exclude is never chosen: moving a
	// session means somewhere other than where it is.
	Owner   string
	Exclude string
}

// The reasons a decision carries.
const (
	ReasonLeastLoad = "least-load"
	ReasonNearLimit = "near-limit"
	ReasonOwner     = "owner"
	ReasonMoved     = "moved"
	ReasonRotation  = "rotation"
	ReasonLast      = "last"
)

// CountedLimit is one row with the figure the rule counted for it.
type CountedLimit struct {
	Limit
	Counted int
	Basis   Basis
}

// Counted is one candidate after counting: what the rule ranked on.
type Counted struct {
	Name         string
	Key          string
	Excluded     string
	Unlaunchable bool
	ObservedAt   int64
	Source       string
	Stale        bool
	Limits       []CountedLimit
	Statuses     []string

	Busy    int
	Pending int
	Load    int

	// Shares splits Busy and Pending by the home they came from, for a
	// surface that says whose they are: the rule counts every home's alike.
	Shares []Share

	// Weekly is the highest counted figure among the rows that are not the
	// session window; Highest is the highest over all rows, with HighestLabel
	// and HighestReset naming it. Each carries the basis of the row it came
	// from, so a surface that prints the figure can say what it rests on.
	Weekly       int
	WeeklyBasis  Basis // "" when the account has no such row
	Highest      int
	HighestBasis Basis // "" when the account has no rows
	HighestLabel string
	HighestReset int64
	NearLimit    bool

	LastPlacedMS int64
	order        int
}

// Share is one home's part of an account's busy sessions and pending launches.
type Share struct {
	Home    string // accounts root
	Busy    int
	Pending int
}

// Elsewhere is the part of the load that came from homes other than home.
func (c Counted) Elsewhere(home string) []Share {
	var out []Share
	for _, s := range c.Shares {
		if s.Home != home {
			out = append(out, s)
		}
	}
	return out
}

// Session returns the session window's row, if the account has one.
func (c Counted) Session() (CountedLimit, bool) {
	for _, l := range c.Limits {
		if l.Session {
			return l, true
		}
	}
	return CountedLimit{}, false
}

// Decision is the rule's answer: the account, why, and every candidate as it
// was counted.
type Decision struct {
	Chosen     string // "" = refused; Refusal says why
	Reason     string
	Refusal    string
	RunnerUp   string // the account the rule would have chosen next
	Rule       string
	Candidates []Counted
}

// Find returns the counted candidate of that name.
func (d Decision) Find(name string) (Counted, bool) {
	for _, c := range d.Candidates {
		if c.Name == name {
			return c, true
		}
	}
	return Counted{}, false
}

// Choose applies the rule. It decides and nothing else: recording the choice
// belongs to the store, which calls this under its lock so that two launches
// cannot both see an account as empty.
func Choose(cands []Candidate, ledger Ledger, intent Intent, now time.Time) Decision {
	d := Decision{Rule: Rule, Candidates: make([]Counted, len(cands))}
	for i, c := range cands {
		d.Candidates[i] = count(c, ledger, i, now)
	}
	switch intent.Kind {
	case Forced:
		c, ok := d.Find(intent.Account)
		switch {
		case !ok:
			d.Refusal = fmt.Sprintf("%q is not a discovered account", intent.Account)
		case c.Unlaunchable:
			d.Refusal = fmt.Sprintf("%s cannot be launched: %s", c.Name, c.Excluded)
		default:
			d.Chosen, d.Reason = c.Name, intent.Reason
		}
	case LastUsed:
		last, ok := ledger.Newest(intent.Home)
		c, found := d.Find(last.Name)
		switch {
		case !ok:
			d.Refusal = "no launch is recorded yet — there is no last account"
		case !found:
			d.Refusal = fmt.Sprintf("the last account used, %s, is no longer a discovered account", last.Name)
		case c.Unlaunchable:
			d.Refusal = fmt.Sprintf("the last account used, %s, cannot be launched: %s", c.Name, c.Excluded)
		default:
			d.Chosen, d.Reason = c.Name, ReasonLast
		}
	default:
		d.auto(intent)
	}
	return d
}

func (d *Decision) auto(intent Intent) {
	var open, near []Counted
	observed := false
	for _, c := range d.Candidates {
		if c.Excluded != "" || c.Name == intent.Exclude {
			continue
		}
		if c.ObservedAt > 0 {
			observed = true
		}
		if c.NearLimit {
			near = append(near, c)
		} else {
			open = append(open, c)
		}
	}
	slices.SortStableFunc(open, func(a, b Counted) int {
		return cmp.Or(
			cmp.Compare(a.Load, b.Load),
			cmp.Compare(a.Weekly, b.Weekly),
			cmp.Compare(a.LastPlacedMS, b.LastPlacedMS),
			cmp.Compare(a.order, b.order),
		)
	})
	slices.SortStableFunc(near, func(a, b Counted) int {
		return cmp.Or(
			cmp.Compare(a.Highest, b.Highest),
			compareReset(a.HighestReset, b.HighestReset),
			cmp.Compare(a.order, b.order),
		)
	})
	ranked := append(slices.Clone(open), near...)
	if len(ranked) == 0 {
		d.Refusal = "no account can take a launch: " + d.exclusions(intent)
		return
	}

	if intent.Owner != "" {
		for _, c := range open {
			if c.Name == intent.Owner {
				d.Chosen, d.Reason = c.Name, ReasonOwner
				for _, other := range ranked {
					if other.Name != c.Name {
						d.RunnerUp = other.Name
						break
					}
				}
				return
			}
		}
	}
	d.Chosen = ranked[0].Name
	if len(ranked) > 1 {
		d.RunnerUp = ranked[1].Name
	}
	switch {
	case len(open) == 0:
		d.Reason = ReasonNearLimit
	case intent.Owner != "":
		d.Reason = ReasonMoved
	case !observed:
		d.Reason = ReasonRotation
	default:
		d.Reason = ReasonLeastLoad
	}
}

// compareReset orders by the sooner reset; an unknown reset sorts last.
func compareReset(a, b int64) int {
	switch {
	case a == b:
		return 0
	case a == 0:
		return 1
	case b == 0:
		return -1
	}
	return cmp.Compare(a, b)
}

// exclusions names why each candidate could not be chosen.
func (d Decision) exclusions(intent Intent) string {
	if len(d.Candidates) == 0 {
		return "no accounts were discovered"
	}
	parts := make([]string, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		why := c.Excluded
		if why == "" && c.Name == intent.Exclude {
			why = "the session is already there"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, why))
	}
	return strings.Join(parts, ", ")
}

func count(c Candidate, ledger Ledger, order int, now time.Time) Counted {
	nowS, nowMS := now.Unix(), now.UnixMilli()
	out := Counted{
		Name: c.Name, Key: c.Key, Excluded: c.Excluded, Unlaunchable: c.Unlaunchable,
		ObservedAt: c.ObservedAt, Source: c.Source, Statuses: c.Statuses, Busy: len(c.Busy), order: order,
		Stale:  c.ObservedAt > 0 && nowS-c.ObservedAt > int64(StaleAfter/time.Second),
		Limits: make([]CountedLimit, 0, len(c.Limits)),
	}
	session := 0
	for i, l := range c.Limits {
		cl := CountedLimit{Limit: l, Counted: l.Percent, Basis: BasisObserved}
		switch {
		case l.Bad:
			// Nothing bounds a percent that did not parse, so it cannot be
			// counted as room: the account is set aside.
			cl.Counted, cl.Basis = 100, BasisBad
		case l.ResetAt != 0 && l.ResetAt <= nowS:
			cl.Counted, cl.Basis = 0, BasisEnded
		case out.Stale:
			cl.Basis = BasisStale
		}
		out.Limits = append(out.Limits, cl)
		if cl.Counted >= NearLimitPercent {
			out.NearLimit = true
		}
		if i == 0 || cl.Counted > out.Highest {
			out.Highest, out.HighestBasis, out.HighestLabel, out.HighestReset = cl.Counted, cl.Basis, l.Label, l.ResetAt
		}
		if l.Session {
			session = cl.Counted
		} else if out.WeeklyBasis == "" || cl.Counted > out.Weekly {
			out.Weekly, out.WeeklyBasis = cl.Counted, cl.Basis
		}
	}
	shares := map[string]*Share{}
	share := func(home string) *Share {
		if shares[home] == nil {
			shares[home] = &Share{Home: home}
		}
		return shares[home]
	}
	for _, b := range c.Busy {
		share(b.Home).Busy++
	}
	cutoff := nowMS - int64(PendingFor/time.Millisecond)
	for _, p := range ledger.Recent {
		if p.Key != c.Key || p.AtMS < cutoff {
			continue
		}
		if isBusy(p, c.Busy) {
			continue // counted once, as the busy session it became
		}
		out.Pending++
		share(p.Home).Pending++
	}
	for _, s := range shares {
		out.Shares = append(out.Shares, *s)
	}
	slices.SortFunc(out.Shares, func(a, b Share) int { return cmp.Compare(a.Home, b.Home) })
	out.Load = max(session, 0)/StepPoints + out.Busy + out.Pending
	out.LastPlacedMS = ledger.Last[c.Key].AtMS
	return out
}

// isBusy reports that a recorded launch is one of the account's busy sessions:
// the same pid, registered no earlier than the launch that became it.
func isBusy(p Pending, busy []Proc) bool {
	for _, b := range busy {
		if b.PID == p.PID && b.StartedMS >= p.AtMS-dedupeSlackMS {
			return true
		}
	}
	return false
}
