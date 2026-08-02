// Package throttle records, per account, when headroom last spent a usage
// request and when it may spend the next one.
//
// The usage endpoint's budget is per *account*, not per machine or per IP —
// account A can be refused in the same second B and C succeed. It refills in
// roughly a minute. That budget is scarce enough that headroom's own surfaces
// were exhausting it against each other: the dashboard, `--json`, `select`,
// `check` and every `watch` round each fetch all accounts, so two of them
// within a minute refuse each other, and because they fan out in parallel the
// whole fleet goes dark at once rather than one account at a time.
//
// Nothing here is secret and nothing here is Claude Code's: the file holds
// timestamps about headroom's own past behaviour. Rows are never stored —
// Claude Code already caches those in .claude.json, and headroom reads them
// from there.
//
// Coordination is best-effort by design. An attempt is recorded *before* the
// request goes out, which closes the ordinary sequential case; two processes
// racing between read and write can still both fetch. The cost of losing that
// race is one wasted request that falls back to cached rows — degraded
// freshness, never a wrong verdict — so a lock or lease would buy correctness
// this program does not need.
package throttle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	// Spacing is the quiet period after any attempt. Deliberately above the
	// ~30-70s refill measured against the live endpoint: headroom is a
	// bystander on an undocumented endpoint and should err toward silence.
	Spacing = 90 * time.Second

	// CooldownBase is the quiet period after a refusal, doubling per
	// consecutive strike up to CooldownMax. Backoff here means *no traffic*:
	// a refused request may itself count against the budget, so probing to
	// discover recovery can prevent the recovery it is probing for.
	CooldownBase = 2 * time.Minute
	CooldownMax  = 16 * time.Minute
)

type record struct {
	LastAttemptMS  int64 `json:"last_attempt_ms"`
	NextEligibleMS int64 `json:"next_eligible_ms"`
	Strikes        int   `json:"strikes,omitempty"`
}

// Store is one file's worth of per-account request history, keyed by account
// name. A missing or corrupt file is an empty store: throttling state is a
// convenience, and losing it must never stop the tool from working.
type Store struct {
	path  string
	recs  map[string]record
	dirty bool
}

// Load reads the store for an accounts root.
func Load(accountsRoot string) *Store {
	s := &Store{path: filepath.Join(accountsRoot, ".throttle"), recs: map[string]record{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var recs map[string]record
	if json.Unmarshal(data, &recs) == nil && recs != nil {
		s.recs = recs
	}
	return s
}

// Eligible reports whether a live request for this account may go out now.
func (s *Store) Eligible(name string, now time.Time) bool {
	r, ok := s.recs[name]
	if !ok {
		return true
	}
	return now.UnixMilli() >= r.NextEligibleMS
}

// NextEligible is when this account may next be fetched; the zero time means
// "now". Surfaces render it so a deferred account explains itself instead of
// looking broken.
func (s *Store) NextEligible(name string) time.Time {
	r, ok := s.recs[name]
	if !ok || r.NextEligibleMS == 0 {
		return time.Time{}
	}
	return time.UnixMilli(r.NextEligibleMS)
}

// NoteAttempt records a request about to be issued, holding the account quiet
// for Spacing. Called before the request so a concurrent process sees the
// claim even though this one has no answer yet.
func (s *Store) NoteAttempt(name string, now time.Time) {
	r := s.recs[name]
	r.LastAttemptMS = now.UnixMilli()
	r.NextEligibleMS = now.Add(Spacing).UnixMilli()
	s.recs[name] = r
	s.dirty = true
}

// NoteRefused records a 429 and lengthens this account's quiet period. Strikes
// accumulate per account: one account being refused says nothing about the
// others, so it must not slow them down.
func (s *Store) NoteRefused(name string, now time.Time) {
	r := s.recs[name]
	r.Strikes++
	cooldown := CooldownBase << (r.Strikes - 1)
	if cooldown > CooldownMax || cooldown <= 0 {
		cooldown = CooldownMax
	}
	r.LastAttemptMS = now.UnixMilli()
	r.NextEligibleMS = now.Add(cooldown).UnixMilli()
	s.recs[name] = r
	s.dirty = true
}

// NoteSuccess clears the strike count. Only a completed request proves the
// budget recovered — a quiet interval alone does not.
func (s *Store) NoteSuccess(name string, now time.Time) {
	r := s.recs[name]
	r.Strikes = 0
	r.LastAttemptMS = now.UnixMilli()
	r.NextEligibleMS = now.Add(Spacing).UnixMilli()
	s.recs[name] = r
	s.dirty = true
}

// Save writes the store atomically (temp file + rename) so a concurrent
// reader never sees a half-written file. Nothing is written when nothing
// changed, and a failure is silently tolerated: throttling is advisory.
func (s *Store) Save() error {
	if !s.dirty {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s.recs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".throttle-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
