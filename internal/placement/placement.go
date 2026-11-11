// Package placement chooses the account a launch runs on. It is one pure
// function over recorded inputs — no clock, file or network of its own — so
// the rule can be tested by table and replaced without touching what gathers
// its inputs or what records its answer.
//
// The rule gives each limit one job. The session window (the vendor's shortest
// limit, five hours for Claude Code) ranks accounts, as load: its usage in
// steps of ten points, plus one step for each busy session and each launch too
// recent to show in the figures. Any limit at or above the near-limit
// threshold sets an account aside. Weekly room breaks ties, measured against
// the time its window has left: room that ends tomorrow goes before room that
// lasts the week. A rule that folds both windows into one figure lets the
// larger weekly number absorb every new launch, and launches then pile onto
// one account — the failure this exists to end.
//
// A figure is counted only for the window it describes. A row whose reset has
// passed counts as zero, a stale observation's rows are lower bounds, and an
// account that cannot be asked is tried rather than avoided: the idle accounts
// are the ones that cannot be asked. Every counted figure carries its basis,
// so a lower bound is never recorded as measured room.
//
// A reservation never reaches the rule: it is decided before it, as an
// ordinary exclusion elsewhere and nothing at all in its repositories, so the
// rule cannot prefer an account for what it is kept for.
//
// A launch that asks for a model with a weekly limit of its own (Fable) is
// admitted, not ranked, by that limit: an account runs it only while its own
// reading puts that limit under ModelLimitPercent, whoever chose the account,
// and with none left the launch refuses. Where it may run, it is ranked like
// any other launch. Admission adds no preference, so launches started
// together still spread.
package placement

import (
	"cmp"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"strings"
	"time"
)

// The constants are policy, chosen and not measured.
const (
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

	// ModelLimitPercent is where a model's own limit stops admitting launches
	// that ask for that model: an account at or above it cannot take one.
	// It sits past NearLimitPercent on purpose — that threshold spreads
	// launches while another account has room; this one refuses outright.
	ModelLimitPercent = 90

	// AssumedWindow is the time a weekly window is counted to have left when
	// nothing dates its next renewal — no reset ahead, no schedule to name
	// the next one, no stated duration — and for an account with no weekly
	// row at all. A whole week is the most a weekly window can have
	// left, so an assumed figure is never more pressing than a measured one.
	AssumedWindow = 7 * 24 * time.Hour

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

// TimeBasis says what a weekly window's counted time left rests on.
type TimeBasis string

const (
	TimeReset     TimeBasis = "reset"     // the vendor's reset instant, still ahead
	TimeProjected TimeBasis = "projected" // the reset has passed: the next one its schedule names
	TimeWindow    TimeBasis = "window"    // the reset has passed or was not given: a whole stated window
	TimeAssumed   TimeBasis = "assumed"   // nothing says: AssumedWindow
)

// Limit is one limit row of an account's newest observation.
type Limit struct {
	Kind    string // the vendor's word for the row
	Label   string // what a person reads
	Percent int
	Bad     bool  // the percent did not parse
	ResetAt int64 // unix seconds; 0 = none
	Window  int64 // the window's length in seconds, as the vendor states it; 0 = not stated
	Period  int64 // seconds between the window's renewals, when it keeps a fixed schedule; 0 = it does not
	Session bool  // the session window

	// Model is the model the row is scoped to, by the vendor's name for it
	// ("Fable"); "" for a row every model spends, and for one whose identity
	// did not parse, which then names no model.
	Model string
}

// Proc identifies a process: pids recycle, so the start instant travels along.
type Proc struct {
	PID       int
	StartedMS int64
}

// Candidate is everything the rule knows about one account.
type Candidate struct {
	Name string // the account's name: what a launch selects and a line prints
	Ref  string // how records name it (accounts.Account.Ref): what a re-home is kept under
	Key  string // the quota it spends; two dirs on one account share a key

	// Excluded says why an automatic choice avoids this account ("" = it may
	// be chosen). Unlaunchable narrows that: no launch can run here at all, so
	// a forced choice refuses too.
	Excluded     string
	Unlaunchable bool

	ObservedAt int64 // unix seconds; 0 = never observed
	Limits     []Limit

	// Busy is every verified-live session the vendor reports as working on
	// this subscription, and every headless run on it: two dirs logged into
	// one account spend one quota.
	Busy []Proc
}

// Pending is one recorded launch.
//
// ID names a headless placement — one whose caller starts the session itself
// and reports its process afterwards (Attach). Until it does, PID is 0, which
// no process carries, so the placement counts as a launch and nothing else.
type Pending struct {
	Key  string `json:"account"`
	Name string `json:"name"`
	PID  int    `json:"pid"`
	AtMS int64  `json:"at_ms"`
	ID   string `json:"id,omitempty"`
}

// Last is the newest launch recorded on one subscription, by the name its
// account had then: for the record and the tie-break, never for routing.
type Last struct {
	Name string `json:"name"`
	AtMS int64  `json:"at_ms"`
}

// Ledger is the record of launches the rule reads and the store keeps: the
// recent ones, which are load, and the newest per subscription, which breaks
// ties.
type Ledger struct {
	Recent []Pending       `json:"recent,omitempty"`
	Last   map[string]Last `json:"last,omitempty"`
}

// Clone is a copy that a later Record or Prune on the original leaves alone.
func (l Ledger) Clone() Ledger {
	return Ledger{Recent: slices.Clone(l.Recent), Last: maps.Clone(l.Last)}
}

// Record adds a launch on an account. id names a headless placement, whose
// session process is attached to it later (Attach); it is "" for a launch,
// whose process is pid. name is what the account was called, for the record.
func (l *Ledger) Record(id, key, name string, pid int, now time.Time) {
	at := now.UnixMilli()
	l.Recent = append(l.Recent, Pending{Key: key, Name: name, PID: pid, AtMS: at, ID: id})
	if l.Last == nil {
		l.Last = map[string]Last{}
	}
	// Newest wins: a record is never replaced by an older one.
	if cur, ok := l.Last[key]; !ok || at >= cur.AtMS {
		l.Last[key] = Last{Name: name, AtMS: at}
	}
}

// Attach gives the placement named id the process that took it, so that once
// the process is counted as busy the placement is not counted beside it. It
// returns the placement, and false when no recent placement has that id.
func (l *Ledger) Attach(id string, pid int, now time.Time) (Pending, bool) {
	if id == "" {
		return Pending{}, false
	}
	for i := range l.Recent {
		if l.Recent[i].ID == id && l.Recent[i].Counts(now) {
			l.Recent[i].PID = pid
			return l.Recent[i], true
		}
	}
	return Pending{}, false
}

// Counts reports that the launch is still load at now: it was recorded within
// PendingFor. It is the one lifetime a placement has — what the rule counts,
// what Prune keeps and what Attach finds — so a placement cannot be over for
// one of them and current for another, whichever ran last.
func (p Pending) Counts(now time.Time) bool {
	return p.AtMS >= now.UnixMilli()-int64(PendingFor/time.Millisecond)
}

// Prune drops what no longer counts: recent launches past their span
// (Counts), and per-account records older than keep. It reports whether
// anything changed.
func (l *Ledger) Prune(now time.Time, keep time.Duration) bool {
	changed := false
	kept := l.Recent[:0]
	for _, p := range l.Recent {
		if p.Counts(now) {
			kept = append(kept, p)
		} else {
			changed = true
		}
	}
	l.Recent = kept
	n := len(l.Last)
	maps.DeleteFunc(l.Last, func(_ string, rec Last) bool { return rec.AtMS < now.Add(-keep).UnixMilli() })
	return changed || len(l.Last) != n
}

// IntentKind is how the account is to be decided.
type IntentKind int

const (
	Auto   IntentKind = iota // the rule chooses
	Forced                   // the caller names the account; the rule records it
)

// Intent is what a launch asks of the rule.
type Intent struct {
	Kind    IntentKind
	Account string // Forced: the account
	Reason  string // Forced: why, for the record ("named", ReasonOwner)

	// Owner is the account that last drove the session this launch names, ""
	// when it names none or none is known.
	Owner string

	// CanWait says the caller would rather wait than take an account near a
	// limit: when every account left is near one, the rule refuses instead of
	// choosing the least spent of them, and says when room returns. A headless
	// caller can wait; the person whose own sessions share the account cannot.
	CanWait bool

	// Models are the models the launch asks for, as its arguments name them
	// ("fable", "claude-fable-5-2"), Forced or Auto alike: naming an account
	// creates no quota. The first that some account's reading has a limit
	// of its own for is the one the launch is admitted by (Decision.Model);
	// the rest, and a name no reading carries, change nothing.
	Models []string
}

// The reasons a decision carries.
const (
	ReasonLeastLoad = "least-load"
	ReasonNearLimit = "near-limit"
	ReasonOwner     = "owner"
	ReasonMoved     = "moved"
	ReasonRotation  = "rotation"
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
	Ref          string
	Key          string
	Excluded     string
	Unlaunchable bool
	ObservedAt   int64
	Stale        bool
	Limits       []CountedLimit

	Busy    int
	Pending int
	Load    int

	// Week is the weekly figure that breaks ties. Highest is the highest
	// counted figure over all rows, with HighestLabel and HighestReset naming
	// it. Each carries the basis of the row it came from, so a surface that
	// prints the figure can say what it rests on.
	Week         Week
	Highest      int
	HighestBasis Basis // "" when the account has no rows
	HighestLabel string
	HighestReset int64
	NearLimit    bool

	// ModelRow is this account's row for the model the launch is admitted by
	// (Decision.Model), its tightest when it has several; nil when the launch
	// asks for no such model or this account's reading carries no row for it.
	// ModelRefusal is why this account cannot take that launch, whoever chose
	// it: the row at or past ModelLimitPercent, or no row at all, since a
	// limit nothing measured is not room. "" when it can.
	ModelRow     *CountedLimit
	ModelRefusal string

	LastPlacedMS int64
	order        int
}

// Week is the weekly row that bounds how fast an account can be driven: of
// the rows that are not the session window, the one with the least room per
// second left — room being what remains below the near-limit threshold. Each
// row is judged against its own window, so a row about to reset never makes
// the account look tighter than a row that has days to run, and spending more
// on any row never makes the account more attractive. An account with no such
// row counts a whole assumed window of room; its Basis is "".
type Week struct {
	Kind    string
	Label   string
	Counted int
	Basis   Basis // "" when the account has no weekly row
	ResetAt int64 // the row's reset as the vendor gave it; 0 = none

	Room      int   // NearLimitPercent minus Counted, never below zero
	Left      int64 // seconds counted until the window ends; at least one
	LeftBasis TimeBasis
}

// pressing compares two weeks by room per second left: positive when a has
// more of its room to spend per second than b, which is the order an
// automatic choice prefers. The products are taken in 128 bits, so no figure
// or window a vendor reports can overflow them.
func (a Week) pressing(b Week) int {
	ah, al := bits.Mul64(uint64(a.Room), uint64(b.Left))
	bh, bl := bits.Mul64(uint64(b.Room), uint64(a.Left))
	return cmp.Or(cmp.Compare(ah, bh), cmp.Compare(al, bl))
}

// week counts one row's room and the time its window has left. A reset that
// has passed names the next one when the window keeps a schedule: an idle
// account's figures can be days old, and counting its renewed window as a
// whole week from now would rank it behind room that in fact lasts longer.
func week(l Limit, counted int, basis Basis, nowS int64) Week {
	w := Week{Kind: l.Kind, Label: l.Label, Counted: counted, Basis: basis, ResetAt: l.ResetAt,
		Room: min(max(NearLimitPercent-counted, 0), NearLimitPercent)}
	switch {
	case l.ResetAt > nowS:
		w.Left, w.LeftBasis = l.ResetAt-nowS, TimeReset
	case l.ResetAt > 0 && l.Period > 0:
		w.Left, w.LeftBasis = l.Period-(nowS-l.ResetAt)%l.Period, TimeProjected
	case l.Window > 0:
		w.Left, w.LeftBasis = l.Window, TimeWindow
	default:
		w.Left, w.LeftBasis = int64(AssumedWindow/time.Second), TimeAssumed
	}
	return w
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
	Candidates []Counted

	// RoomAt is when the first account set aside near a limit has room again,
	// in unix seconds, on a refusal to a caller that can wait; 0 when no such
	// account says when its limits reset. On a refusal for want of a model's
	// own limit (Model set), it is when that limit first renews on an account
	// it alone held back.
	RoomAt int64

	// Model is the model whose own limit admitted this launch, by the
	// vendor's name ("Fable"); "" when the launch asks for none such.
	Model string
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
	d := Decision{Candidates: make([]Counted, len(cands)), Model: limitedModel(cands, intent.Models)}
	for i, c := range cands {
		d.Candidates[i] = count(c, ledger, i, now, d.Model)
	}
	switch intent.Kind {
	case Forced:
		c, ok := d.Find(intent.Account)
		switch {
		case !ok:
			d.Refusal = fmt.Sprintf("%q is not a discovered account", intent.Account)
		case c.Unlaunchable:
			d.Refusal = fmt.Sprintf("%s cannot be launched: %s", c.Name, c.Excluded)
		case c.ModelRefusal != "":
			d.Refusal = fmt.Sprintf("%s cannot take a %s launch: %s", c.Name, d.Model, c.ModelRefusal)
			if c.ModelRow != nil && c.ModelRow.ResetAt > now.Unix() {
				d.RoomAt = c.ModelRow.ResetAt
			}
		default:
			d.Chosen, d.Reason = c.Name, intent.Reason
		}
	default:
		d.auto(intent, now)
	}
	return d
}

// limitedModel is the first model the launch asks for that some account's
// reading has a limit of its own for, by the vendor's name; "" when none.
// That a model has such a limit is read from the readings, never assumed: a
// vendor that drops a model's own limit — as it once did for Opus — leaves
// launches asking for it placed like any other, where a name written into
// the rule would refuse every one of them.
func limitedModel(cands []Candidate, asked []string) string {
	for _, a := range asked {
		f := family(a)
		if f == "" {
			continue
		}
		for _, c := range cands {
			for _, l := range c.Limits {
				if l.Model != "" && family(l.Model) == f {
					return l.Model
				}
			}
		}
	}
	return ""
}

// family is a model's name reduced to the word that names its family, so a
// launch's alias meets the vendor's name for the row: "fable", "Fable",
// "claude-fable-5-2", "fable[1m]" and "Claude Fable 5" are all "fable". A
// name that starts with no letters is "", which matches nothing.
func family(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{"claude-", "claude "} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.IndexFunc(s, func(r rune) bool { return r < 'a' || r > 'z' }); i >= 0 {
		s = s[:i]
	}
	return s
}

// modelRefusal is why an account cannot take a launch admitted by model's own
// limit, "" when it can. No row is a refusal: a limit nothing measured on
// this account is not room, and the launch it would let through is the one
// this exists to stop.
func modelRefusal(row *CountedLimit, model string) string {
	switch {
	case row == nil:
		return "no " + model + " figure"
	case row.Basis == BasisBad:
		return row.Label + " unreadable"
	case row.Counted < ModelLimitPercent:
		return ""
	case row.Basis == BasisStale:
		return fmt.Sprintf("%s ≥%d%%", row.Label, row.Counted)
	}
	return fmt.Sprintf("%s %d%%", row.Label, row.Counted)
}

func (d *Decision) auto(intent Intent, now time.Time) {
	var open, near []Counted
	observed := false
	for _, c := range d.Candidates {
		if c.Excluded != "" {
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
			b.Week.pressing(a.Week),
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
		if d.heldByModel() {
			d.Refusal, d.RoomAt = d.modelRefusal(), d.modelRoomAt(now)
			return
		}
		d.Refusal = "no account can take a launch: " + d.exclusions()
		return
	}
	if intent.CanWait && len(open) == 0 {
		d.Refusal, d.RoomAt = d.nearRefusal(near), roomAt(near)
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

// heldBack reports that the model's own limit is what keeps this account out
// of the launch: nothing else excludes it.
func (c Counted) heldBack() bool {
	return c.ModelRefusal != "" && c.Excluded == c.ModelRefusal
}

// heldByModel reports that some account the rule could otherwise consider was
// kept out by the model's own limit alone.
func (d Decision) heldByModel() bool {
	return slices.ContainsFunc(d.Candidates, Counted.heldBack)
}

// modelRefusal says why a launch asking for a model with its own limit was
// refused: the accounts that limit held back, each with its figure, then the
// others with why they could not be chosen at all.
func (d Decision) modelRefusal() string {
	var held, others []string
	for _, c := range d.Candidates {
		if c.heldBack() {
			held = append(held, fmt.Sprintf("%s (%s)", c.Name, c.ModelRefusal))
		} else {
			others = append(others, fmt.Sprintf("%s (%s)", c.Name, c.Excluded))
		}
	}
	why := "no account has room on " + d.Model + ": " + strings.Join(held, ", ")
	if len(others) > 0 {
		why += "; not eligible: " + strings.Join(others, ", ")
	}
	return why
}

// modelRoomAt is when the model's own limit first renews on an account it
// alone held back; 0 when none of them says.
func (d Decision) modelRoomAt(now time.Time) int64 {
	var first int64
	for _, c := range d.Candidates {
		if !c.heldBack() || c.ModelRow == nil || c.ModelRow.ResetAt <= now.Unix() {
			continue
		}
		if at := c.ModelRow.ResetAt; first == 0 || at < first {
			first = at
		}
	}
	return first
}

// nearRefusal says why a caller that can wait was refused: every account it
// could take is near a limit, each named with its highest figure, and the
// others with why they could not be chosen at all.
func (d Decision) nearRefusal(near []Counted) string {
	parts := make([]string, 0, len(near))
	for _, c := range near {
		parts = append(parts, fmt.Sprintf("%s (%s %d%%)", c.Name, c.HighestLabel, c.Highest))
	}
	why := "every account that could take it is near a limit: " + strings.Join(parts, ", ")
	var others []string
	for _, c := range d.Candidates {
		if c.Excluded != "" {
			others = append(others, fmt.Sprintf("%s (%s)", c.Name, c.Excluded))
		}
	}
	if len(others) > 0 {
		why += "; not eligible: " + strings.Join(others, ", ")
	}
	return why
}

// roomAt is when the first of these accounts leaves the near-limit set; 0
// when none says.
func roomAt(near []Counted) int64 {
	var first int64
	for _, c := range near {
		if at := c.RoomAt(); at > 0 && (first == 0 || at < first) {
			first = at
		}
	}
	return first
}

// RoomAt is when an account near a limit leaves the near-limit set, in unix
// seconds: once every row at or past the threshold has reset, so the latest
// of those resets. 0 when the account is not near a limit, or a row at the
// threshold names no reset or has a percent that did not parse — nothing
// then says when.
func (c Counted) RoomAt() int64 {
	var at int64
	for _, l := range c.Limits {
		if l.Counted < NearLimitPercent {
			continue
		}
		if l.Basis == BasisBad || l.ResetAt == 0 {
			return 0
		}
		at = max(at, l.ResetAt)
	}
	return at
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
func (d Decision) exclusions() string {
	if len(d.Candidates) == 0 {
		return "no accounts were discovered"
	}
	parts := make([]string, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, c.Excluded))
	}
	return strings.Join(parts, ", ")
}

// count is one candidate as the rule ranks it. model is the model whose own
// limit admits this launch (Decision.Model), "" for none: the account's row
// for it is kept, and an account it cannot take is excluded like any other
// an automatic choice avoids, behind whatever already excluded it.
func count(c Candidate, ledger Ledger, order int, now time.Time, model string) Counted {
	nowS, nowMS := now.Unix(), now.UnixMilli()
	out := Counted{
		Name: c.Name, Ref: c.Ref, Key: c.Key, Excluded: c.Excluded, Unlaunchable: c.Unlaunchable,
		ObservedAt: c.ObservedAt, Busy: len(c.Busy), order: order,
		Stale:  c.ObservedAt > 0 && nowS-c.ObservedAt > int64(StaleAfter/time.Second),
		Limits: make([]CountedLimit, 0, len(c.Limits)),
		Week:   week(Limit{}, 0, "", nowS),
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
		} else if w := week(l, cl.Counted, cl.Basis, nowS); out.Week.Basis == "" || w.pressing(out.Week) < 0 {
			out.Week = w
		}
		if model != "" && l.Model != "" && family(l.Model) == family(model) &&
			(out.ModelRow == nil || cl.Counted > out.ModelRow.Counted) {
			out.ModelRow = &out.Limits[len(out.Limits)-1]
		}
	}
	if model != "" {
		out.ModelRefusal = modelRefusal(out.ModelRow, model)
		if out.Excluded == "" {
			out.Excluded = out.ModelRefusal
		}
	}
	for _, p := range ledger.Recent {
		if p.Key != c.Key || !p.Counts(time.UnixMilli(nowMS)) {
			continue
		}
		if isBusy(p, c.Busy) {
			continue // counted once, as the busy session it became
		}
		out.Pending++
	}
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
