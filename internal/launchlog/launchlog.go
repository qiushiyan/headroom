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
	Week       Week     `json:"week"`
	LastPlaced *string  `json:"last_placed_at,omitempty"`

	// Homes splits busy and pending by the home they came from, by accounts
	// root: another home holding logins of the same subscription puts its
	// sessions and launches on the same count.
	Homes []Share `json:"homes,omitempty"`
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
		if c.ObservedAt > 0 {
			lc.ObservedAt = stamp(time.Unix(c.ObservedAt, 0))
		}
		if c.LastPlacedMS > 0 {
			lc.LastPlaced = stamp(time.UnixMilli(c.LastPlacedMS))
		}
		for _, sh := range c.Shares {
			lc.Homes = append(lc.Homes, Share{Home: sh.Home, Busy: sh.Busy, Pending: sh.Pending})
		}
		for _, l := range c.Limits {
			ll := Limit{Kind: l.Kind, Label: l.Label, Percent: l.Percent, WindowS: l.Window, Session: l.Session, Counted: l.Counted, Basis: string(l.Basis)}
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

// ErrBusy is Append's answer when the log is being rewritten and stayed so past
// the wait: the record was not written, and the caller says so.
var ErrBusy = errors.New("launch log is busy")

const (
	appendWait = 250 * time.Millisecond
	lockPoll   = 10 * time.Millisecond
)

// Append writes one record as one line, in one write, so concurrent appenders
// interleave whole lines. A failure is the caller's to mention and never to
// act on: a launch that could not be logged is still a launch.
//
// Appending and the rewrite that bounds the file are one protocol on the log's
// own lock: appenders share it, the rewrite holds it alone. Without that, a
// rewrite renames a new file over the one an appender has just written to, and
// a launch that succeeded leaves no line. The state lock is never involved —
// routing must not wait on a file it does not read — and an appender waits
// only as long as a rewrite takes, then gives up and reports it.
func Append(accountsRoot string, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(accountsRoot, 0o755); err != nil {
		return err
	}
	path := Path(accountsRoot)
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := flock(lock, syscall.LOCK_SH, appendWait); err != nil {
		return err
	}
	size, err := appendLine(path, line)
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err == nil && size > MaxBytes {
		prune(path, lock, time.Now())
	}
	return err
}

// appendLine writes the record and reports the file's size afterwards. A file
// that does not end in a newline holds a line some writer never finished; the
// record then starts on a line of its own, so the damage costs the damaged
// line and not the launch after it.
func appendLine(path string, line []byte) (int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	out := make([]byte, 0, len(line)+2)
	if fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			out = append(out, '\n')
		}
	}
	out = append(append(out, line...), '\n')
	if _, err := f.Write(out); err != nil {
		return 0, err
	}
	return fi.Size() + int64(len(out)), nil
}

// flock takes the lock in the given mode, polling for at most wait.
func flock(f *os.File, how int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrBusy
		}
		time.Sleep(lockPoll)
	}
}

// prune rewrites the log without the lines older than Keep, holding the log's
// lock alone so no appender writes to the file being replaced. It does not
// wait for the lock: an appender holding it is about to make the same check,
// and a rewrite skipped now happens at a later append. A line that does not
// parse, or carries no time, is dropped with the old ones.
func prune(path string, lock *os.File, now time.Time) {
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
	for _, line := range bytes.Split(data, []byte("\n")) {
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
		kept.WriteByte('\n')
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
