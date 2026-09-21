// Package accountstate assembles account facts from disk. It has no credential,
// health-probe or network dependencies.
package accountstate

import (
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Health answers: can Claude Code use this account at all? Only /login fixes
// a bad answer here.
type Health int

const (
	HealthOK              Health = iota // logged in and usable
	HealthNoLogin                       // never logged in on this config dir
	HealthReloginRequired               // refresh token demonstrably expired
	HealthBadBlob                       // credential present but off-contract
	HealthUnknown                       // nothing could establish it either way

	// HealthUnprobed means this surface skipped the health question.
	// HealthUnknown means it asked but could not establish an answer; an
	// offline read must not let an unasked question read as a failed probe.
	HealthUnprobed
)

// Source identifies the origin of an observation.
type Source int

const (
	SourceLive  Source = iota // headroom's own fetch, in this process
	SourceStore               // headroom's own fetch, replayed from the state file
	SourceCache               // Claude Code's own cache in .claude.json
)

// Ours groups this process's response and a stored response for captions:
// both were fetched by headroom at the observation's timestamp. JSON keeps
// them distinct so consumers can tell which run actually asked.
func (s Source) Ours() bool { return s == SourceLive || s == SourceStore }

// Observation keeps rows with the provenance that makes them interpretable.
// Rows without their original timestamp can make carried-over usage look current.
type Observation struct {
	Rows       []usage.Row
	ObservedAt int64 // unix seconds
	Source     Source
}

// AttemptState is what happened the last time headroom tried to refresh —
// about the *request*, never about the account.
type AttemptState int

const (
	AttemptNone       AttemptState = iota // no attempt this run
	AttemptPending                        // in flight
	AttemptOK                             // fresh rows landed
	AttemptRefused                        // HTTP 429 — says nothing about the account
	AttemptDeferred                       // not attempted: still inside a quiet period
	AttemptTokenStale                     // access token aged out; a session refreshes it
	// A credential read failure blocks our request independently of health.
	AttemptCredentialUnreadable
	AttemptTransport   // network or timeout
	AttemptHTTP        // some other non-200
	AttemptUnparseable // 200 whose body failed the contract
	AttemptNoLimits    // 200 reporting no limit windows
	// No request was authorized because its bookkeeping was unavailable.
	AttemptStateUnavailable
	// An unreadable identity cannot authorize a per-account budget.
	AttemptIdentityUnknown
)

// Attempt is the outcome of the newest refresh, with the time the account
// becomes eligible again where one applies.
type Attempt struct {
	State          AttemptState
	HTTPCode       int
	NextEligibleAt int64  // unix seconds; 0 = eligible now
	StoreError     string // request bookkeeping failed independently of the endpoint result
}

// FreshWindow follows the request spacing: no newer answer is obtainable
// inside that period. It is a display policy, not a vendor promise.
// Sharing the spacing prevents a timing change from marking every other run
// stale while no newer answer is obtainable. Spacing is per vendor, so facts
// carry their own window (Facts.FreshFor, set at assembly from the scope);
// this is the default for facts assembled without one.
const FreshWindow = config.DefaultSpacing

// Facts carries three independent answers: an account can be logged in,
// have figures 22 hours old, and have its newest refresh refused. Keeping
// them separate prevents a 429 or stale token from erasing known usage.
type Facts struct {
	Vendor      config.Vendor
	FreshFor    time.Duration // the scope's spacing; 0 means FreshWindow
	Label       string
	DirMismatch string // dir name when the logged-in email doesn't match it
	Plan        string
	Launcher    string
	Current     bool // a bare launch (no --account) targets this account
	Health      Health
	Obs         *Observation // nil = nothing known
	Attempt     Attempt
}

// Fresh reports whether the observation is recent enough to describe current
// headroom.
func (v Facts) Fresh(now int64) bool {
	window := v.FreshFor
	if window <= 0 {
		window = FreshWindow
	}
	return v.Obs != nil && now-v.Obs.ObservedAt <= int64(window/time.Second)
}

// Actionable is the question the picker actually asks: are these figures
// grounds for choosing this account right now? Freshness alone is not —
// a logged-out account can hold a cache minutes old, and offering it as a
// live choice on that basis is the mistake this model exists to prevent.
//
// Nor is a fresh observation enough on its own. A row whose own reset instant
// has passed describes a window that has since ended: the percent is a fact
// about the past, and a *low* one reads as headroom that may not exist.
func (v Facts) Actionable(now int64) bool {
	return v.Health == HealthOK && v.Fresh(now) && !v.RolledOver(now)
}

// RolledOver reports that some row has outlived the window it describes.
func (v Facts) RolledOver(now int64) bool {
	if v.Obs == nil {
		return false
	}
	for _, r := range v.Obs.Rows {
		if r.RolledOver(now) {
			return true
		}
	}
	return false
}
