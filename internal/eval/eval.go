// Package eval judges automatic placement by what followed it. It reads the
// two logs headroom keeps and never routes by — the launch log, what each
// launch was shown and what it chose, and the usage log, what every
// account's windows said afterwards — and reports, per rule version, how the
// choices and the accounts fared.
//
// Two kinds of answer come out, and they mean different things:
//
//   - Outcomes are observed: which windows reached a limit, how long they sat
//     there while another account had room, how much weekly room may have
//     renewed unspent, and how high the chosen account's session window went
//     after each automatic launch. They are what actually happened — under
//     everything that happened on the machine, since a pinned launch spends
//     quota too, and never what a different choice would have caused. A
//     window counts under the rule whose automatic launch last went into it;
//     one no automatic launch went into counts as Unplaced. Time across the
//     whole machine is reported as context, owned by no rule.
//   - The replay is counterfactual: each automatic launch's recorded input is
//     decided again by the rule this binary was built with. It says which
//     choices a changed rule would have made differently on the same input —
//     never what the usage would then have been. A line decided by the same
//     rule that the replay answers differently is a line the input could not
//     be rebuilt from, or a rule that changed without a new name.
//
// Every figure is a reading's, at its time, and says which way it bounds the
// truth: a percent read at one moment is at least that much afterwards, room
// is room only on a fresh reading or a window that has since ended, and an
// account nobody could ask has no readings — its silence is reported as
// unobserved, never as idle.
package eval

import (
	"cmp"
	"slices"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/usagelog"
)

// The judging policy: chosen, not measured, like the rule's own constants, and
// printed with every report so a figure says what it was counted against.
const (
	// NearPercent is the figure at which a window counts as near its limit:
	// the rule's own threshold, so "near" means what the rule sets aside.
	NearPercent = placement.NearLimitPercent
	FullPercent = 100

	// RoomBelow is the session figure under which another account counts as
	// having had room for the work that went elsewhere.
	RoomBelow = 50

	// FreshFor is how old a figure may be and still describe its account at a
	// moment: twice the usage log's heartbeat.
	FreshFor = 2 * usagelog.Heartbeat

	// Step is the grid the time measures are taken on.
	Step = 5 * time.Minute

	// Follow is how long after an automatic launch its account's session
	// window is followed — and how long before its reset a window that states
	// no length (every Claude Code session window) is taken to have begun at
	// most: a launch further back than that went into an earlier window.
	Follow = 6 * time.Hour

	// eligibleFor is how long a launch line's word on whether an account
	// could be chosen is taken to hold.
	eligibleFor = 24 * time.Hour

	// assumedWeek is the span a weekly window is taken to cover when its
	// readings state none.
	assumedWeek = int64(7 * 24 * time.Hour / time.Second)
)

// Launch is one launch line and the home whose log held it.
type Launch struct {
	Home string
	launchlog.Record
}

// Input is everything one report is made from.
type Input struct {
	Vendor config.Vendor
	Home   string // the accounts root the report is made from: whose names it prefers

	Launches []Launch
	Usage    []usagelog.Record

	// Keys names the subscription of an account a launch line knows only by
	// name — every version 1 line — as home + "\x00" + name.
	Keys map[string]string

	// Skipped counts the lines of either log that did not parse; Problems
	// names what could not be read at all. Both travel to the report: partial
	// evidence is usable only while it says it is partial.
	Skipped  int
	Problems []string

	Since time.Time // zero: everything
	Now   time.Time
}

// Report is the evaluation of one vendor's placements.
type Report struct {
	Vendor     string     `json:"vendor"`
	Home       string     `json:"home"` // the accounts root the report was made from
	Rule       string     `json:"rule"` // the rule this binary decides by: what the replay ran
	Since      *string    `json:"since"`
	Until      string     `json:"until"`
	Thresholds Thresholds `json:"thresholds"`
	Sources    Sources    `json:"sources"`

	// Rules summarizes each rule version, oldest first: its decisions, and the
	// windows its automatic launches went into. Unplaced holds the windows no
	// automatic launch went into — pinned, named and resumed work, or work no
	// launch logged. Fleet is time across every account, owned by no rule.
	Rules    []RuleSummary `json:"rules"`
	Unplaced RuleSummary   `json:"unplaced"`
	Fleet    FleetStats    `json:"fleet"`

	// The evidence behind the summaries, oldest first.
	Episodes  []Episode  `json:"episodes"`
	Renewals  []Renewal  `json:"renewals"`
	Decisions []Decision `json:"decisions"`
}

type Thresholds struct {
	NearPercent int   `json:"near_percent"`
	RoomBelow   int   `json:"room_below_percent"`
	StaleAfterS int64 `json:"stale_after_s"` // what "chosen on old figures" counts against: the rule's own
	FreshForS   int64 `json:"fresh_for_s"`
	StepS       int64 `json:"step_s"`
	FollowS     int64 `json:"follow_s"`
}

type Sources struct {
	Launches int      `json:"launches"` // launch lines read, every home
	Homes    int      `json:"homes"`    // homes whose launch log held a line
	Logged   int      `json:"usage_log_readings"`
	Carried  int      `json:"readings_from_launches"` // figures carried in launch lines that the usage log does not hold
	Unkeyed  int      `json:"unkeyed_accounts"`       // accounts a launch line named that no key could be found for
	Skipped  int      `json:"skipped_lines"`
	Problems []string `json:"problems"` // what could not be read: the report is partial while this is not empty
	First    *string  `json:"first"`    // the earliest line of either log
	Last     *string  `json:"last"`
}

// RuleSummary is one rule version's record: what it decided, and the windows
// its automatic launches went into.
type RuleSummary struct {
	Rule string `json:"rule"`
	From string `json:"from"` // its first launch line, in any home
	To   string `json:"to"`   // its last

	Launches LaunchStats  `json:"launches"`
	Outcomes OutcomeStats `json:"outcomes"`
	Session  LimitStats   `json:"session_windows"`
	Weekly   WeeklyStats  `json:"weekly_windows"`
	Replay   ReplayStats  `json:"replay"`
}

type LaunchStats struct {
	Total     int            `json:"total"`
	ByMode    map[string]int `json:"by_mode"`
	Automatic int            `json:"automatic"`
	ByReason  map[string]int `json:"by_reason"`  // automatic launches
	ByAccount map[string]int `json:"by_account"` // automatic launches, by subscription, named as the report's home names it

	// LongestRun is the most automatic launches in a row on one subscription.
	LongestRun int `json:"longest_run"`

	// What the chosen account's figures were when it was chosen.
	ChosenStale      int   `json:"chosen_stale"`      // older than placement.StaleAfter
	ChosenUnobserved int   `json:"chosen_unobserved"` // never observed
	ChosenAgeP50S    int64 `json:"chosen_age_p50_s"`
	ChosenAgeP90S    int64 `json:"chosen_age_p90_s"`
}

// OutcomeStats follows each automatic launch's chosen account, and the
// runner-up beside it, through the session window the launch landed in.
type OutcomeStats struct {
	Followed         int     `json:"followed"`     // automatic launches whose account's window was read afterwards
	ReachedNear      int     `json:"reached_near"` // ... whose window then reached NearPercent
	Exhausted        int     `json:"exhausted"`    // ... reached 100
	MeanRise         float64 `json:"mean_rise"`    // mean points from the figure decided on to the window's peak
	RunnerUpFollowed int     `json:"runner_up_followed"`
	RunnerUpNear     int     `json:"runner_up_reached_near"`
}

// LimitStats counts windows by how far they went, and how long they sat near
// their limit.
type LimitStats struct {
	ReachedNear   int   `json:"reached_near"`
	Exhausted     int   `json:"exhausted"`
	RoomElsewhere int   `json:"room_elsewhere"` // reached near while another account had room
	NearS         int64 `json:"near_s"`         // time from reaching near to the reset (or now)
	SqueezedS     int64 `json:"squeezed_s"`     // ... of which another account had room
}

// WeeklyStats are LimitStats and the renewals. A renewal's figure is the last
// read before it, so usage is a lower bound and the room that renewed unspent
// an upper one.
type WeeklyStats struct {
	LimitStats
	Renewals            int     `json:"renewals"`
	MeanUsedMin         float64 `json:"mean_used_min"`
	LapsedMaxPts        int     `json:"lapsed_max_pts"`
	LapsedWhileSqueezed int     `json:"lapsed_while_squeezed"` // renewals with room possibly left while another account's same limit reached near
}

// FleetStats is time across every account, taken on the Step grid and owned
// by no rule: which rule a moment belongs to is not something the logs say.
type FleetStats struct {
	ObservedS     int64   `json:"observed_s"` // grid time some account's figures were fresh
	NearS         int64   `json:"near_s"`     // ... with a session window at or above NearPercent
	SqueezedS     int64   `json:"squeezed_s"` // ... while another account had room
	MeanSpreadPts float64 `json:"mean_spread_pts"`

	// UnobservedRenewals are weekly renewals a window's schedule implies with
	// no reading in them: neither spent nor lapsed as far as anyone knows.
	UnobservedRenewals int `json:"unobserved_renewals"`
}

type ReplayStats struct {
	Replayed int  `json:"replayed"` // automatic launches decided again
	Agreed   int  `json:"agreed"`
	Inexact  int  `json:"inexact"`   // lines whose whole input is not on record
	Faults   int  `json:"faults"`    // see Replay.Fault
	SameRule bool `json:"same_rule"` // the lines were decided by the rule the replay ran
}

// Episode is one window that reached the near-limit threshold.
type Episode struct {
	Account string `json:"account"`
	Key     string `json:"key"`
	Limit   string `json:"limit"`
	Session bool   `json:"session"`

	// Rule is the rule whose automatic launch last went into the window
	// before NearAt, and the window counts under it; Placed is false, and Rule
	// empty, when none did.
	Rule   string `json:"rule"`
	Placed bool   `json:"placed"`

	NearAt      string  `json:"near_at"` // the first reading at or above NearPercent
	ExhaustedAt *string `json:"exhausted_at"`
	Peak        int     `json:"peak"`
	ResetsAt    string  `json:"resets_at"`
	NearS       int64   `json:"near_s"`     // NearAt to the reset, or to now
	SqueezedS   int64   `json:"squeezed_s"` // ... of which another account had room

	// Launches is every launch into the window up to NearAt, by how it was
	// decided ("auto" for the rule's choices), and the same launches by the
	// accounts root that made them.
	Launches map[string]int `json:"launches"`
	ByHome   map[string]int `json:"launches_by_home"`

	// Elsewhere is the other account with the most room at NearAt; null when
	// none had room.
	Elsewhere *Room `json:"room_elsewhere"`
}

// Room is another account with room at a moment, and what says so: a fresh
// reading ("fresh"), or a session window that has ended since an older one
// ("ended") — room unless it was used where nobody looked.
type Room struct {
	Account string `json:"account"`
	Session int    `json:"session"` // its session figure, 0 once the window ended
	Highest int    `json:"highest"` // its highest figure on any limit, as counted
	AgeS    int64  `json:"age_s"`   // how old the reading was
	Basis   string `json:"basis"`
}

// Renewal is one weekly window observed to its end. The figure is the last
// read before the renewal: usage at least that, room left at most the rest.
type Renewal struct {
	Account   string `json:"account"`
	Key       string `json:"key"`
	Limit     string `json:"limit"` // the limit read highest at the renewal: the one that bound the account
	Rule      string `json:"rule"`  // the rule whose automatic launch last went into the window; "" when none did
	Placed    bool   `json:"placed"`
	RenewedAt string `json:"renewed_at"`
	UsedMin   int    `json:"used_min"`
	LastReadS int64  `json:"last_read_s"` // how long before the renewal that figure was read
	LapsedMax int    `json:"lapsed_max"`  // 100 - UsedMin

	// PeakElsewhere is the highest any other account's same limit read
	// during this window.
	PeakElsewhere int `json:"peak_elsewhere"`
}

// Decision is one automatic launch, what followed it, and the replay's answer.
// Names are the line's own: the account as the home that launched it calls it.
type Decision struct {
	At       string `json:"at"`
	Home     string `json:"home"`
	Rule     string `json:"rule"`
	Mode     string `json:"mode"`
	Reason   string `json:"reason"`
	Chosen   string `json:"chosen"`
	RunnerUp string `json:"runner_up,omitempty"`

	ChosenAfter   *After `json:"chosen_after"` // null: the window was not read afterwards
	RunnerUpAfter *After `json:"runner_up_after"`
	Replay        Replay `json:"replay"`
}

// After is one account's session window from a launch on: the window that was
// open at the launch, or the first to open after it.
type After struct {
	Counted  int    `json:"counted"` // the figure the launch was decided on in this window; 0 when the window opened after it
	Peak     int    `json:"peak"`
	PeakAt   string `json:"peak_at"`
	ResetsAt string `json:"resets_at"`
}

// Replay is the launch decided again by this binary's rule.
type Replay struct {
	Chosen string `json:"chosen"` // the line's name for it; "" = the rule refused
	Reason string `json:"reason"`
	Agrees bool   `json:"agrees"`
	Exact  bool   `json:"exact"` // the line carried the decision's whole input

	// Fault marks a disagreement that cannot be a difference between rules:
	// the line was decided by the rule the replay ran, its whole input on
	// record. Either the line is missing an input the rule reads, or the rule
	// changed without a new name — and every figure this report credits to
	// that name mixes two rules.
	Fault bool `json:"fault"`

	// After is the replayed choice's session window as it was observed — with
	// the launch on the other account, not as it would have been with it.
	After *After `json:"after,omitempty"`
}

// Build makes the report.
func Build(in Input) Report {
	e := newEvaluator(in)
	rep := Report{
		Vendor: string(in.Vendor), Home: in.Home, Rule: placement.Rule, Until: stamp(in.Now.Unix()),
		Thresholds: Thresholds{
			NearPercent: NearPercent, RoomBelow: RoomBelow, StaleAfterS: int64(placement.StaleAfter / time.Second),
			FreshForS: int64(FreshFor / time.Second), StepS: int64(Step / time.Second), FollowS: int64(Follow / time.Second),
		},
		Sources:   e.sources,
		Episodes:  []Episode{},
		Renewals:  []Renewal{},
		Decisions: []Decision{},
	}
	if !in.Since.IsZero() {
		s := stamp(in.Since.Unix())
		rep.Since = &s
	}
	for _, l := range e.launches {
		s := e.rule(l.Rule)
		if s.From == "" {
			s.From = l.At
		}
		s.To = l.At
	}

	e.decisions(&rep)
	e.windows(&rep)
	rep.Fleet = e.fleet()

	for _, s := range e.byRule {
		s.Replay.SameRule = s.Rule == placement.Rule
		s.Weekly.MeanUsedMin = round1(s.Weekly.MeanUsedMin)
		rep.Rules = append(rep.Rules, *s)
	}
	slices.SortStableFunc(rep.Rules, func(a, b RuleSummary) int { return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Rule, b.Rule)) })
	e.unplaced.Weekly.MeanUsedMin = round1(e.unplaced.Weekly.MeanUsedMin)
	rep.Unplaced = *e.unplaced
	return rep
}

func newSummary(rule string) *RuleSummary {
	return &RuleSummary{Rule: rule, Launches: LaunchStats{ByMode: map[string]int{}, ByReason: map[string]int{}, ByAccount: map[string]int{}}}
}

// ---- small things ----

func minute(s int64) int64 { return (s + 30) / 60 * 60 }

// parse reads one of the logs' times; RFC3339 with or without fractions.
func parse(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *s)
	return t, err == nil
}

func unix(s *string) int64 {
	if t, ok := parse(s); ok {
		return t.Unix()
	}
	return 0
}

func stamp(s int64) string { return time.Unix(s, 0).UTC().Format(time.RFC3339) }

func round1(f float64) float64 {
	if f < 0 {
		return -round1(-f)
	}
	return float64(int64(f*10+0.5)) / 10
}
