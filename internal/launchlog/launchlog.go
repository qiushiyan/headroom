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
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// Version is the record shape this binary writes.
const Version = 1

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
	At     string `json:"at"` // RFC3339 UTC
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
	Eligible   bool     `json:"eligible"`
	Excluded   string   `json:"excluded,omitempty"`
	NearLimit  bool     `json:"near_limit"`
	ObservedAt *string  `json:"observed_at"` // RFC3339 UTC; null = never observed
	Source     string   `json:"source,omitempty"`
	Limits     []Limit  `json:"limits"`
	Statuses   []string `json:"statuses"` // every live session's status, as the vendor wrote it
	Busy       int      `json:"busy"`
	Pending    int      `json:"pending"`
	Load       int      `json:"load"`
	Weekly     int      `json:"weekly"`
	LastPlaced *string  `json:"last_placed_at,omitempty"`
}

// Limit is one row: what the vendor said and what the rule counted for it.
type Limit struct {
	Kind     string  `json:"kind"`
	Label    string  `json:"label,omitempty"`
	Percent  int     `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	Session  bool    `json:"session,omitempty"`
	Counted  int     `json:"counted"`
	Basis    string  `json:"basis"`
}

// New builds a record from a decision. What the decision does not know — the
// vendor, the process, how the account came to be decided — is the caller's to
// add.
func New(d placement.Decision, at time.Time) Record {
	r := Record{
		V: Version, At: at.UTC().Format(time.RFC3339),
		Reason: d.Reason, Rule: d.Rule, Chosen: d.Chosen, RunnerUp: d.RunnerUp,
		Candidates: make([]Candidate, 0, len(d.Candidates)),
	}
	for _, c := range d.Candidates {
		lc := Candidate{
			Name: c.Name, Eligible: c.Excluded == "", Excluded: c.Excluded, NearLimit: c.NearLimit,
			Source: c.Source, Statuses: c.Statuses, Busy: c.Busy, Pending: c.Pending, Load: c.Load, Weekly: c.Weekly,
			Limits: make([]Limit, 0, len(c.Limits)),
		}
		if lc.Statuses == nil {
			lc.Statuses = []string{}
		}
		if c.ObservedAt > 0 {
			lc.ObservedAt = stamp(time.Unix(c.ObservedAt, 0))
		}
		if c.LastPlacedMS > 0 {
			lc.LastPlaced = stamp(time.UnixMilli(c.LastPlacedMS))
		}
		for _, l := range c.Limits {
			ll := Limit{Kind: l.Kind, Label: l.Label, Percent: l.Percent, Session: l.Session, Counted: l.Counted, Basis: string(l.Basis)}
			if l.ResetAt > 0 {
				ll.ResetsAt = stamp(time.Unix(l.ResetAt, 0))
			}
			lc.Limits = append(lc.Limits, ll)
		}
		r.Candidates = append(r.Candidates, lc)
	}
	return r
}

func stamp(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// Append writes one record as one line, in one write, so concurrent appenders
// interleave whole lines. A failure is the caller's to mention and never to
// act on: a launch that could not be logged is still a launch.
func Append(accountsRoot string, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(accountsRoot, 0o755); err != nil {
		return err
	}
	path := Path(accountsRoot)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	fi, serr := f.Stat()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil && serr == nil && fi.Size() > MaxBytes {
		prune(path, time.Now())
	}
	return werr
}

// prune rewrites the log without the lines older than Keep. It takes the log's
// own lock without waiting — whoever holds it is already doing this — and
// never the state lock: routing must not wait on a file it does not read. A
// line that does not parse, or carries no time, is dropped with the old ones.
// An append racing the rename can lose its line; the log is a record for a
// person, and a launch never depended on it.
func prune(path string, now time.Time) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	cutoff := now.Add(-Keep)
	var kept bytes.Buffer
	dropped := false
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			At string `json:"at"`
		}
		at, perr := time.Time{}, json.Unmarshal(line, &head)
		if perr == nil {
			at, perr = time.Parse(time.RFC3339, head.At)
		}
		if perr != nil || at.Before(cutoff) {
			dropped = true
			continue
		}
		kept.Write(line)
	}
	if !dropped {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "launches-*.jsonl")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(kept.Bytes()); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

// Read returns the newest n records, oldest first; n <= 0 means all of them.
// A line that does not parse is skipped and counted: a torn final line during a
// concurrent append is ordinary. An absent file is an empty log.
func Read(accountsRoot string, n int) (records []Record, skipped int, err error) {
	data, err := os.ReadFile(Path(accountsRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil || r.At == "" {
			skipped++
			continue
		}
		records = append(records, r)
	}
	if n > 0 && len(records) > n {
		records = records[len(records)-n:]
	}
	return records, skipped, nil
}
