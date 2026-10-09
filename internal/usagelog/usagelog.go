// Package usagelog keeps the record of what the usage endpoint said: one line
// per reading headroom fetched, in the accounts root whose state.json holds
// the subscription ledger it was fetched for.
//
// It is the outcome side of the launch log. A launch line says what the rule
// was shown and what it chose; these lines say what then happened to every
// account's windows, so a placement can be judged by what followed it. Like
// the launch log it explains and is never an input: nothing that routes,
// claims or renders reads it, so deleting it, corrupting it or holding its
// lock changes nothing headroom does.
//
// A line holds the decoded rows, not the body: the usage parser stays the only
// reader of vendor bodies, and this file, headroom's own document, has Read
// as its only reader. A body would be several times the size of its rows, and
// the file gains a line per fetch.
//
// Volume is bounded twice. A reading identical to the last one this process
// logged for the account is skipped until that line is placement.StaleAfter
// old: a board left open asks every account once a minute, and an unchanged
// figure says nothing new until the rule itself would call it stale. A gap
// between two lines of an account longer than that is therefore time nobody
// observed it. And the file is bounded by age once it grows, as the launch log
// is.
package usagelog

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/jsonl"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Version is the record shape this binary writes.
const Version = 1

// The file is bounded by age once it has grown past MaxBytes. It keeps less
// than the launch log: figures from a quarter ago describe a rule long
// replaced. Variables so a test can make a small file "large".
var (
	MaxBytes int64 = 16 << 20
	Keep           = 90 * 24 * time.Hour
)

// Heartbeat is how old the last line of an unchanged account may grow before
// an identical reading is logged again.
const Heartbeat = placement.StaleAfter

// Path is the log beside the subscription ledger in that accounts root.
func Path(ledgerRoot string) string { return filepath.Join(ledgerRoot, "usage.jsonl") }

// Record is one reading.
type Record struct {
	V      int    `json:"v"`
	At     string `json:"at"` // RFC3339 UTC: when the answer arrived
	Vendor string `json:"vendor"`

	// Key is the subscription the reading measured, as the ledger keys it.
	// Name and Home are the account dir and the home that asked: two homes'
	// logins of one subscription log under one key and their own names.
	Key  string `json:"key"`
	Name string `json:"name"`
	Home string `json:"home"`

	// Allowance is the vendor's account-level word, "" when it said nothing
	// either way — which is every Claude Code reading.
	Allowance string `json:"allowance,omitempty"`
	Rows      []Row  `json:"rows"`
}

// Row is one limit row as the parser decoded it: its identity, its figure and
// its reset, and which fields were present but did not parse.
type Row struct {
	Kind      string   `json:"kind"`
	Group     string   `json:"group,omitempty"`
	Model     string   `json:"model,omitempty"`
	Feature   string   `json:"feature,omitempty"`
	WindowS   int64    `json:"window_s,omitempty"`
	Label     string   `json:"label,omitempty"`
	Percent   int      `json:"percent"`
	ResetsAt  *string  `json:"resets_at"` // RFC3339 UTC; null = none stated, or see Bad
	Unstarted bool     `json:"unstarted,omitempty"`
	Bad       []string `json:"bad,omitempty"` // "percent", "reset", "identity"
}

// New builds the record of one reading.
func New(at time.Time, vendor config.Vendor, key, name, home string, rows []usage.Row, allowance usage.Allowance) Record {
	r := Record{
		V: Version, At: at.UTC().Format(time.RFC3339), Vendor: string(vendor),
		Key: key, Name: name, Home: home, Rows: make([]Row, 0, len(rows)),
	}
	if allowance.State != usage.AllowanceUnknown {
		r.Allowance = allowance.State.Name()
	}
	for _, u := range rows {
		row := Row{
			Kind: u.Kind, Group: u.Group, Model: u.Model, Feature: u.Feature, WindowS: u.WindowSeconds,
			Label: u.Label, Percent: u.Percent, Unstarted: u.Unstarted,
		}
		if u.ResetAt > 0 {
			s := time.Unix(u.ResetAt, 0).UTC().Format(time.RFC3339)
			row.ResetsAt = &s
		}
		for _, f := range []struct {
			name  string
			state usage.FieldState
		}{{"percent", u.PercentState}, {"reset", u.ResetState}, {"identity", u.IdentityState}} {
			if f.state == usage.StateBad {
				row.Bad = append(row.Bad, f.name)
			}
		}
		r.Rows = append(r.Rows, row)
	}
	return r
}

// Usage is the row as the usage package holds it, so the vendor's own rules —
// which row is the session window, which bound ordinary work, how a window
// renews — read a logged row exactly as they read a live one.
func (r Row) Usage() usage.Row {
	u := usage.Row{
		Kind: r.Kind, Group: r.Group, Model: r.Model, Feature: r.Feature, WindowSeconds: r.WindowS,
		Label: r.Label, Percent: r.Percent, Unstarted: r.Unstarted,
		PercentState: usage.StateOK, ResetState: usage.StateNone, IdentityState: usage.StateOK,
	}
	if r.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *r.ResetsAt); err == nil {
			u.ResetAt, u.ResetState = t.Unix(), usage.StateOK
		}
	}
	if slices.Contains(r.Bad, "percent") {
		u.PercentState, u.Percent = usage.StateBad, 0
	}
	if slices.Contains(r.Bad, "reset") {
		u.ResetState, u.ResetAt = usage.StateBad, 0
	}
	if slices.Contains(r.Bad, "identity") {
		u.IdentityState = usage.StateBad
	}
	return u
}

func file(ledgerRoot string) jsonl.Log {
	return jsonl.Log{Path: Path(ledgerRoot), MaxBytes: MaxBytes, Keep: Keep, Slack: Keep / 10}
}

// logged is the last line this process wrote per log and account: what an
// identical reading is compared against.
var (
	mu     sync.Mutex
	logged = map[string]last{}
)

type last struct {
	digest string
	at     time.Time
}

// Append writes one reading, unless it repeats the last line this process
// logged for the account and that line is younger than Heartbeat. A failure
// is the caller's to drop: a reading that could not be logged is a gap the
// log's reader sees as unobserved time, and nothing else.
func Append(ledgerRoot string, r Record) error {
	at, err := time.Parse(time.RFC3339, r.At)
	if err != nil {
		return file(ledgerRoot).Append(r)
	}
	body := struct {
		Allowance string `json:"allowance"`
		Rows      []Row  `json:"rows"`
	}{r.Allowance, r.Rows}
	b, _ := json.Marshal(body)
	id := Path(ledgerRoot) + "\x00" + r.Key
	mu.Lock()
	prev, seen := logged[id]
	mu.Unlock()
	if seen && prev.digest == string(b) && at.Sub(prev.at) < Heartbeat && !at.Before(prev.at) {
		return nil
	}
	if err := file(ledgerRoot).Append(r); err != nil {
		return err
	}
	mu.Lock()
	logged[id] = last{digest: string(b), at: at}
	mu.Unlock()
	return nil
}

// Read returns every record, oldest first. A line that does not parse, or
// names no time or no account, is skipped and counted. An absent file is an
// empty log.
func Read(ledgerRoot string) (records []Record, skipped int, err error) {
	return jsonl.Read(Path(ledgerRoot), func(r Record) bool { return r.At != "" && r.Key != "" })
}
