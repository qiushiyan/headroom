package usage

import (
	"time"

	"github.com/qiushiyan/headroom/internal/config"
)

// AllowanceState is the account-level answer a usage response gives to "may
// this account be used at all", beside whatever its windows say. The zero
// value is Unknown, and Unknown is never read as allowed.
type AllowanceState uint8

const (
	AllowanceUnknown AllowanceState = iota // the response says nothing either way — every Claude Code response
	AllowanceAllowed
	AllowanceBlocked // positive, well-typed evidence that the vendor will refuse work
	AllowanceBad     // a blocking field present under a type it has never had — drift, never a block
)

// Name is the wire spelling used by --json.
func (s AllowanceState) Name() string {
	switch s {
	case AllowanceAllowed:
		return "allowed"
	case AllowanceBlocked:
		return "blocked"
	case AllowanceBad:
		return "bad"
	default:
		return "unknown"
	}
}

// Allowance is what changes whether the account is usable, kept apart from
// the windows: a reached limit is carried here, not by a severity word
// headroom would have to invent. BlockedFeatures names limits blocked on
// their own (code review, an additional metered feature); they are said in a
// caption and never change State.
type Allowance struct {
	State           AllowanceState
	Reason          string // the vendor's words for the block: its reason string, else the field that said so
	BlockedFeatures []string
}

// Reading is everything one usage body says: its windows, its allowance, and
// whose it claims to be (each "" when the body does not say), so no caller
// decodes a body a second time.
type Reading struct {
	Rows      []Row
	Allowance Allowance
	AccountID string
	UserID    string
	Plan      string
}

// BelongsTo is the response identity check: a body may be shown under an
// account only if it does not name someone else. Its account_id must equal
// the account's, and its user_id the user's when the body carries one. A body
// that names no account at all is accepted — the request was already bound to
// the account by its header — which is every Claude Code body.
func (r Reading) BelongsTo(accountID, userID string) bool {
	if r.AccountID != "" && r.AccountID != accountID {
		return false
	}
	if r.UserID != "" && r.UserID != userID {
		return false
	}
	return true
}

// Drifted reports whether anything in the reading was present but no longer
// parses: a row's field, or the allowance.
func (r Reading) Drifted() int {
	n := 0
	for _, row := range r.Rows {
		if row.Drifted() {
			n++
		}
	}
	if r.Allowance.State == AllowanceBad {
		n++
	}
	return n
}

// SessionWindow is the index of the vendor's shortest limit among rows: Claude
// Code's `session` row, and the shortest window of Codex's main rate limit. -1
// when the reading holds none it can identify. Which row is the short window is
// the vendor's vocabulary, so it is decided here, beside the parsers, and
// nowhere else. A row whose identity failed the contract is never it.
func SessionWindow(vendor config.Vendor, rows []Row) int {
	best := -1
	for i, r := range rows {
		if r.IdentityState == StateBad {
			continue
		}
		if vendor == config.Codex {
			if r.Group != CodexGroupMain || r.WindowSeconds <= 0 {
				continue
			}
			if best < 0 || r.WindowSeconds < rows[best].WindowSeconds {
				best = i
			}
			continue
		}
		if r.Kind == "session" {
			return i
		}
	}
	return best
}

// Period is how often a row's window renews on a fixed schedule, for a window
// the vendor is known to renew so; 0 for every other. Claude Code's weekly
// windows renew every seven days at the same instant whether or not anything
// was spent since, so a reset that has passed still names the next one
// (observed on four accounts in October 2026: each reset advanced by exactly
// seven days, unused accounts included). Its five-hour window starts with
// the first request after one ends, and so does every Codex window: none of
// those keeps a schedule a passed reset could name.
func Period(vendor config.Vendor, r Row) time.Duration {
	if vendor == config.Codex || r.IdentityState == StateBad {
		return 0
	}
	if r.Kind == "weekly_all" || r.Kind == "weekly_scoped" {
		return 7 * 24 * time.Hour
	}
	return 0
}

// General reports whether a row bounds ordinary work on the account, as
// opposed to one feature of it. Every Claude Code row does. Of Codex's, only
// the main rate limit's windows do: a code-review or additional limit that is
// spent, or that no longer parses, blocks that feature and leaves the account
// usable, so it must not decide where a session goes.
func General(vendor config.Vendor, r Row) bool {
	return vendor != config.Codex || r.Group == CodexGroupMain
}

// Parse is the one dispatch from a vendor to the parser of its usage body.
// Live interpretation, replay from the store and `check` all come through
// here: the store a body was read from, or the endpoint it was fetched from,
// says which vendor it is, and nothing else may guess.
func Parse(vendor config.Vendor, body []byte) (Reading, error) {
	if vendor == config.Codex {
		return parseCodex(body)
	}
	rows, err := ParseLimits(body)
	return Reading{Rows: rows}, err
}
