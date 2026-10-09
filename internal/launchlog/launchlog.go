// Package launchlog keeps the record of launches: one line per launch, with
// every input the choice was made from.
//
// It explains decisions. It is never an input to one — no routing code reads
// this file, so deleting it, corrupting it or holding its lock changes no
// launch — and it cannot show what a different rule would have cost: replaying
// a rule over these lines yields the decisions it would have made on the
// recorded inputs, not the usage that would have followed.
package launchlog

import (
	"path/filepath"
	"time"

	"github.com/qiushiyan/headroom/internal/jsonl"
	"github.com/qiushiyan/headroom/internal/placement"
)

// Version is the record shape this binary writes. Version 1 carried the
// candidates as the rule counted them and nothing more: no account keys, no
// intent beyond the mode, busy sessions and recent launches as counts, and
// times to the second. A reader of a version 1 line knows a candidate only by
// its name in the home that logged it, cannot always tell an automatic choice
// from a forced one, and cannot rebuild the counts a different rule would
// have made.
const Version = 2

// The file is bounded by age, and only once it has grown past MaxBytes: a
// rewrite on every append would be pure contention for a file that gains a few
// kilobytes a launch. Variables so a test can make a small file "large".
var (
	MaxBytes int64 = 8 << 20
	Keep           = 180 * 24 * time.Hour
)

// Path is the log of one vendor's accounts root.
func Path(accountsRoot string) string { return filepath.Join(accountsRoot, "launches.jsonl") }

// Record is one launch.
type Record struct {
	V      int    `json:"v"`
	At     string `json:"at"`    // RFC3339 UTC
	AtMS   int64  `json:"at_ms"` // the same instant to the millisecond: the clock the decision counted recent launches by
	Vendor string `json:"vendor"`
	PID    int    `json:"pid"`
	CWD    string `json:"cwd"`

	// Mode is how the account came to be decided: "auto", "pinned", "named",
	// "last" or "picker". Reason is the rule's own word for its choice.
	Mode     string `json:"mode"`
	Reason   string `json:"reason"`
	Rule     string `json:"rule"`
	Chosen   string `json:"chosen"`
	RunnerUp string `json:"runner_up,omitempty"`

	// Automatic says the rule chose. Owner and Exclude are the rest of what
	// it was asked: the account that last drove the session the launch
	// names, and the account it must not choose. Recent is the record of
	// launches the rule read, before this one joined it. Together with the
	// candidates' busy processes and last placements they are the decision's
	// whole input, so a rule can be run over this line again — including one
	// that counts load differently.
	Automatic bool                `json:"automatic"`
	Owner     string              `json:"owner,omitempty"`
	Exclude   string              `json:"exclude,omitempty"`
	Recent    []placement.Pending `json:"recent"`

	// Session is the session id the launch named, "" when it named none.
	Session string `json:"session"`

	// Recorded reports that the placement reached the record the next launch
	// reads; Problem says why it did not.
	Recorded bool   `json:"recorded"`
	Problem  string `json:"problem,omitempty"`

	Candidates []Candidate `json:"candidates"`
}

// Candidate is one account as the rule counted it.
type Candidate struct {
	Name       string   `json:"name"`
	Key        string   `json:"key"` // the subscription's ledger key: what joins this line to the usage log
	Eligible   bool     `json:"eligible"`
	Excluded   string   `json:"excluded,omitempty"`
	NearLimit  bool     `json:"near_limit"`
	ObservedAt *string  `json:"observed_at"` // RFC3339 UTC; null = never observed
	Source     string   `json:"source,omitempty"`
	Limits     []Limit  `json:"limits"`
	Statuses   []string `json:"statuses"` // every live session's status, as the vendor wrote it
	Busy       int      `json:"busy"`
	BusyProcs  []Proc   `json:"busy_procs"` // the processes behind Busy, before the rule merged them with recent launches
	Pending    int      `json:"pending"`
	Load       int      `json:"load"`
	Weekly     int      `json:"weekly"`
	Week       Week     `json:"week"`
	LastPlaced *string  `json:"last_placed_at,omitempty"` // RFC3339 UTC to the millisecond: a tie between two accounts is broken by it

	// Homes splits busy and pending by the home they came from, by accounts
	// root: another home holding logins of the same subscription puts its
	// sessions and launches on the same count.
	Homes []Share `json:"homes,omitempty"`
}

// Proc is one busy session's process: pids recycle, so its start travels with
// it, and Home is the accounts root whose registry reported it.
type Proc struct {
	PID       int    `json:"pid"`
	StartedMS int64  `json:"started_ms"`
	Home      string `json:"home,omitempty"`
}

// Share is one home's part of a candidate's busy sessions and pending launches.
type Share struct {
	Home    string `json:"home"`
	Busy    int    `json:"busy"`
	Pending int    `json:"pending"`
}

// Week is the weekly row that broke ties, as the rule counted it: its room
// below the near-limit threshold and the seconds its window was counted to
// have left, with what that time rests on. An account with no weekly row has
// no kind and an assumed window.
type Week struct {
	Kind      string `json:"kind,omitempty"`
	Label     string `json:"label,omitempty"`
	Counted   int    `json:"counted"`
	Room      int    `json:"room"`
	LeftS     int64  `json:"left_s"`
	LeftBasis string `json:"left_basis"`
}

// Limit is one row: what the vendor said and what the rule counted for it.
type Limit struct {
	Kind     string  `json:"kind"`
	Label    string  `json:"label,omitempty"`
	Percent  int     `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	WindowS  int64   `json:"window_s,omitempty"`
	PeriodS  int64   `json:"period_s,omitempty"`
	Session  bool    `json:"session,omitempty"`
	Counted  int     `json:"counted"`
	Basis    string  `json:"basis"`
}

// New builds a record from a decision and everything it was decided on: the
// candidates as the rule was handed them, the record of launches it read, and
// what it was asked. What none of them knows — the vendor, the process, how
// the account came to be decided — is the caller's to add.
func New(d placement.Decision, cands []placement.Candidate, ledger placement.Ledger, intent placement.Intent, at time.Time) Record {
	r := Record{
		V: Version, At: at.UTC().Format(time.RFC3339), AtMS: at.UnixMilli(),
		Reason: d.Reason, Rule: d.Rule, Chosen: d.Chosen, RunnerUp: d.RunnerUp,
		Automatic: intent.Kind == placement.Auto, Owner: intent.Owner, Exclude: intent.Exclude,
		Recent:     append([]placement.Pending{}, ledger.Recent...),
		Candidates: make([]Candidate, 0, len(d.Candidates)),
	}
	for i, c := range d.Candidates {
		lc := Candidate{
			Name: c.Name, Key: c.Key, Eligible: c.Excluded == "", Excluded: c.Excluded, NearLimit: c.NearLimit,
			Source: c.Source, Statuses: c.Statuses, Busy: c.Busy, Pending: c.Pending, Load: c.Load, Weekly: c.Week.Counted,
			Week: Week{
				Kind: c.Week.Kind, Label: c.Week.Label, Counted: c.Week.Counted,
				Room: c.Week.Room, LeftS: c.Week.Left, LeftBasis: string(c.Week.LeftBasis),
			},
			Limits: make([]Limit, 0, len(c.Limits)),
		}
		if lc.Statuses == nil {
			lc.Statuses = []string{}
		}
		// The rule counts its candidates in the order it was handed them.
		lc.BusyProcs = []Proc{}
		if i < len(cands) && cands[i].Name == c.Name {
			for _, p := range cands[i].Busy {
				lc.BusyProcs = append(lc.BusyProcs, Proc{PID: p.PID, StartedMS: p.StartedMS, Home: p.Home})
			}
		}
		if c.ObservedAt > 0 {
			lc.ObservedAt = stamp(time.Unix(c.ObservedAt, 0))
		}
		if c.LastPlacedMS > 0 {
			s := time.UnixMilli(c.LastPlacedMS).UTC().Format(millis)
			lc.LastPlaced = &s
		}
		for _, sh := range c.Shares {
			lc.Homes = append(lc.Homes, Share{Home: sh.Home, Busy: sh.Busy, Pending: sh.Pending})
		}
		for _, l := range c.Limits {
			ll := Limit{Kind: l.Kind, Label: l.Label, Percent: l.Percent, WindowS: l.Window, PeriodS: l.Period, Session: l.Session, Counted: l.Counted, Basis: string(l.Basis)}
			if l.ResetAt > 0 {
				ll.ResetsAt = stamp(time.Unix(l.ResetAt, 0))
			}
			lc.Limits = append(lc.Limits, ll)
		}
		r.Candidates = append(r.Candidates, lc)
	}
	return r
}

// millis is RFC3339 to the millisecond, for the times a decision compares
// between candidates.
const millis = "2006-01-02T15:04:05.000Z07:00"

func stamp(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// ErrBusy is Append's answer when the log is being rewritten and stayed so past
// the wait: the record was not written, and the caller says so.
var ErrBusy = jsonl.ErrBusy

// file is the log of one accounts root, bounded as the variables say now.
func file(accountsRoot string) jsonl.Log {
	return jsonl.Log{Path: Path(accountsRoot), MaxBytes: MaxBytes, Keep: Keep, Slack: Keep / 10}
}

// Append writes one record as one line. A failure is the caller's to mention
// and never to act on: a launch that could not be logged is still a launch.
// The state lock is never involved — routing must not wait on a file it does
// not read.
func Append(accountsRoot string, r Record) error {
	return file(accountsRoot).Append(r)
}

// Read returns the newest n records, oldest first; n <= 0 means all of them.
// A line that does not parse is skipped and counted: a torn final line during a
// concurrent append is ordinary. An absent file is an empty log.
func Read(accountsRoot string, n int) (records []Record, skipped int, err error) {
	records, skipped, err = jsonl.Read(Path(accountsRoot), func(r Record) bool { return r.At != "" })
	if n > 0 && len(records) > n {
		records = records[len(records)-n:]
	}
	return records, skipped, err
}
