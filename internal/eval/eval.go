// Package eval judges automatic placement by what followed it. It reads the
// two logs headroom keeps and never routes by — the launch log, what each
// launch was shown and what it chose, and the usage log, what every
// account's windows said afterwards — and reports, per rule version, how the
// choices and the accounts fared.
//
// Two kinds of answer come out, and they mean different things:
//
//   - Outcomes are observed: which windows reached a limit while another
//     account had room, how long a session window sat near its limit, how
//     much weekly room lapsed unspent, and how high the chosen account's
//     session window went after each automatic launch. They are what actually
//     happened — under everything that happened on the machine, since a
//     pinned launch spends quota too. A rule is charged only for what it
//     placed: time while its home's bare launches went automatic, and windows
//     an automatic launch went into. The rest is counted apart, under Pinned,
//     whatever rule the binary carried.
//   - The replay is counterfactual: each automatic launch's recorded input is
//     decided again by the rule this binary was built with. It says which
//     choices a changed rule would have made differently on the same input —
//     never what the usage would then have been. A line decided by the same
//     rule that the replay answers differently is a line the input could not
//     be rebuilt from, or a rule that changed without a new name.
//
// Every figure is a reading's, at its time. A reading's percent holds until the
// next; an account nobody could ask has no readings, and its silence is
// reported as unobserved, never as idle.
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
	// window is followed.
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

	// Skipped counts the lines of either log that did not parse.
	Skipped int

	Since time.Time // zero: everything
	Now   time.Time
}

// Report is the evaluation of one vendor's placements.
type Report struct {
	Vendor     string     `json:"vendor"`
	Home       string     `json:"home"` // the accounts root the report was made from: whose rule time is attributed to
	Rule       string     `json:"rule"` // the rule this binary decides by: what the replay ran
	Since      *string    `json:"since"`
	Until      string     `json:"until"`
	Thresholds Thresholds `json:"thresholds"`
	Sources    Sources    `json:"sources"`

	// Rules summarizes each rule version, oldest first. Pinned holds what no
	// rule placed: time while the home's bare launches were pinned to an
	// account, and windows no automatic launch went into.
	Rules  []RuleSummary `json:"rules"`
	Pinned RuleSummary   `json:"pinned"`

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
	Launches int     `json:"launches"` // launch lines read, every home
	Homes    int     `json:"homes"`    // homes whose launch log held a line
	Logged   int     `json:"usage_log_readings"`
	Carried  int     `json:"readings_from_launches"` // figures carried in launch lines that the usage log does not hold
	Unkeyed  int     `json:"unkeyed_accounts"`       // accounts a launch line named that no key could be found for
	Skipped  int     `json:"skipped_lines"`
	First    *string `json:"first"` // the earliest line of either log
	Last     *string `json:"last"`
}

// RuleSummary is one rule version's record: what it decided, and what was
// observed while it was in force and bare launches went automatic.
type RuleSummary struct {
	Rule string `json:"rule"`
	From string `json:"from"` // its first launch line in the report's home
	To   string `json:"to"`   // its last

	Launches LaunchStats  `json:"launches"`
	Outcomes OutcomeStats `json:"outcomes"`
	Time     TimeStats    `json:"time"`
	Session  LimitStats   `json:"session_windows"`
	Weekly   WeeklyStats  `json:"weekly_windows"`
	Replay   ReplayStats  `json:"replay"`
}

type LaunchStats struct {
	Total     int            `json:"total"`
	ByMode    map[string]int `json:"by_mode"`
	Automatic int            `json:"automatic"`
	ByReason  map[string]int `json:"by_reason"`  // automatic launches
	ByAccount map[string]int `json:"by_account"` // automatic launches

	// LongestRun is the most automatic launches in a row on one account.
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
	Followed         int     `json:"followed"`     // automatic launches whose account was read afterwards
	ReachedNear      int     `json:"reached_near"` // ... whose session window then reached NearPercent
	Exhausted        int     `json:"exhausted"`    // ... reached 100
	MeanRise         float64 `json:"mean_rise"`    // mean points from the counted figure to the window's peak
	RunnerUpFollowed int     `json:"runner_up_followed"`
	RunnerUpNear     int     `json:"runner_up_reached_near"`
}

// TimeStats are taken on the Step grid.
type TimeStats struct {
	ObservedS int64 `json:"observed_s"` // grid time some account's figures were fresh
	NearS     int64 `json:"near_s"`     // ... with a session window at or above NearPercent
	SqueezedS int64 `json:"squeezed_s"` // ... while another account had room
	// MeanSpreadPts is the mean gap between the highest and lowest fresh
	// session figure, over grid points with two or more fresh accounts.
	MeanSpreadPts float64 `json:"mean_spread_pts"`
}

// LimitStats counts windows by how far they went.
type LimitStats struct {
	ReachedNear   int `json:"reached_near"`
	Exhausted     int `json:"exhausted"`
	RoomElsewhere int `json:"room_elsewhere"` // reached near while another account had room
}

type WeeklyStats struct {
	LimitStats
	Renewals      int     `json:"renewals"` // weekly windows observed to their renewal
	MeanUsed      float64 `json:"mean_used"`
	LapsedPts     int     `json:"lapsed_pts"`     // room that renewed unspent, summed over renewals
	LapsedSqueeze int     `json:"lapsed_squeeze"` // renewals with room left while another account's same limit reached near
	Unobserved    int     `json:"unobserved"`     // renewals a window's schedule implies with no reading in them
}

type ReplayStats struct {
	Replayed int  `json:"replayed"` // automatic launches decided again
	Agreed   int  `json:"agreed"`
	Inexact  int  `json:"inexact"`   // lines whose input could not be rebuilt whole
	SameRule bool `json:"same_rule"` // the lines were decided by the rule the replay ran
}

// Episode is one window that reached the near-limit threshold.
type Episode struct {
	Account     string  `json:"account"`
	Key         string  `json:"key"`
	Limit       string  `json:"limit"`
	Session     bool    `json:"session"`
	Rule        string  `json:"rule"`
	Placed      bool    `json:"placed"` // an automatic launch went into it: Rule is that launch's, and it counts there; else under Pinned, Rule the rule in force
	NearAt      string  `json:"near_at"`
	ExhaustedAt *string `json:"exhausted_at"`
	Peak        int     `json:"peak"`
	ResetsAt    string  `json:"resets_at"`

	// Launches is every launch onto the account from the window's start to
	// NearAt, by how it was decided ("auto" for the rule's choices).
	Launches map[string]int `json:"launches"`
	ByHome   map[string]int `json:"launches_by_home"` // the same launches, by the accounts root that made them

	// Elsewhere is the other account with the most room at NearAt; null when
	// none had room.
	Elsewhere *Room `json:"room_elsewhere"`
}

// Room is another account as it stood at a moment.
type Room struct {
	Account string `json:"account"`
	Session int    `json:"session"` // its session window's figure, 0 once the window ended
	Highest int    `json:"highest"` // its highest figure on any limit
	AgeS    int64  `json:"age_s"`   // how old that reading was
}

// Renewal is one weekly window observed to its end.
type Renewal struct {
	Account   string `json:"account"`
	Key       string `json:"key"`
	Limit     string `json:"limit"`
	Rule      string `json:"rule"`   // the rule in force when it renewed
	Placed    bool   `json:"placed"` // an automatic launch went onto the account in it
	RenewedAt string `json:"renewed_at"`
	Used      int    `json:"used"`        // the last figure read in it — a lower bound
	LastReadS int64  `json:"last_read_s"` // how long before the renewal that figure was read
	Lapsed    int    `json:"lapsed"`      // 100 - Used: room that renewed unspent, at most

	// PeakElsewhere is the highest any other account's same limit read
	// during this window.
	PeakElsewhere int `json:"peak_elsewhere"`
}

// Decision is one automatic launch, what followed it, and the replay's answer.
type Decision struct {
	At       string `json:"at"`
	Home     string `json:"home"`
	Rule     string `json:"rule"`
	Mode     string `json:"mode"`
	Reason   string `json:"reason"`
	Chosen   string `json:"chosen"`
	RunnerUp string `json:"runner_up,omitempty"`

	ChosenAfter   *After `json:"chosen_after"` // null: not read afterwards
	RunnerUpAfter *After `json:"runner_up_after"`
	Replay        Replay `json:"replay"`
}

// After is one account's session window from a launch on.
type After struct {
	Counted  int    `json:"counted"` // the figure the launch was decided on
	Peak     int    `json:"peak"`
	PeakAt   string `json:"peak_at"`
	ResetsAt string `json:"resets_at"`
}

// Replay is the launch decided again by this binary's rule.
type Replay struct {
	Chosen string `json:"chosen"` // "" = the rule refused
	Reason string `json:"reason"`
	Agrees bool   `json:"agrees"`
	Exact  bool   `json:"exact"` // the line carried the decision's whole input
	// After is what the replayed choice's session window did, when it differs.
	After *After `json:"after,omitempty"`
}

// Build makes the report.
func Build(in Input) Report {
	e := newEvaluator(in)
	rep := Report{
		Vendor: string(in.Vendor), Home: in.Home, Rule: placement.Rule, Until: stamp(in.Now.Unix()),
		Thresholds: Thresholds{
			NearPercent: NearPercent, RoomBelow: RoomBelow, StaleAfterS: int64(placement.StaleAfter / time.Second),
			FreshForS: int64(FreshFor / time.Second),
			StepS:     int64(Step / time.Second), FollowS: int64(Follow / time.Second),
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
	// Each rule's span is where it was in force: in the report's home, so a
	// rule another home still runs has the span it had here.
	for _, sp := range e.spans {
		s := e.rule(sp.rule)
		if s.From == "" {
			s.From = stamp(sp.at)
		}
		s.To = stamp(sp.at)
	}

	e.decisions(&rep)
	e.windows(&rep)
	e.grid()

	for _, s := range e.byRule {
		s.Replay.SameRule = s.Rule == placement.Rule
		s.Weekly.MeanUsed = round1(s.Weekly.MeanUsed)
		rep.Rules = append(rep.Rules, *s)
	}
	slices.SortStableFunc(rep.Rules, func(a, b RuleSummary) int { return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Rule, b.Rule)) })
	e.pinned.Weekly.MeanUsed = round1(e.pinned.Weekly.MeanUsed)
	rep.Pinned = *e.pinned
	return rep
}

func newSummary(rule string) *RuleSummary {
	return &RuleSummary{Rule: rule, Launches: LaunchStats{ByMode: map[string]int{}, ByReason: map[string]int{}, ByAccount: map[string]int{}}}
}

// ---- small things ----

func minute(s int64) int64 { return (s + 30) / 60 * 60 }

func unix(s *string) int64 {
	if s == nil {
		return 0
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func stamp(s int64) string { return time.Unix(s, 0).UTC().Format(time.RFC3339) }

func round1(f float64) float64 {
	if f < 0 {
		return -round1(-f)
	}
	return float64(int64(f*10+0.5)) / 10
}
