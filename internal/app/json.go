package app

// The dashboard for machines: same pipeline, same tagged degradation,
// serialized instead of drawn. Consumers get the drift tags as explicit
// state strings — a bad field is distinguishable from a zero or an absent
// one, exactly as in the human view. Field changes bump "schema".

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

type jsonDoc struct {
	Schema      int    `json:"schema"`
	GeneratedAt string `json:"generated_at"` // RFC3339 UTC
	// Current is keyed by vendor and holds only the vendors in this document:
	// the account name a bare launch of that vendor targets ("" = none
	// resolvable). The same email can be an account of both vendors, so one
	// string could not say which.
	Current  map[string]string `json:"current"`
	Accounts []jsonAccount     `json:"accounts"` // one flat list; select by "vendor"

	// Problems are defects in headroom's own state file, never statements
	// about Claude Code. They exist so a machine consumer can tell "nothing
	// has ever been observed" from "something is on disk and this run could
	// not read it" — without them, an unreadable store and a first run both
	// serialize as usage:null, and the second is a silence that lies.
	Problems []jsonProblem `json:"problems,omitempty"`
}

type jsonProblem struct {
	Vendor  string `json:"vendor"` // whose state file
	Section string `json:"section"`
	Detail  string `json:"detail"`
}

// The account's three axes are three fields, for the same reason they are
// three fields in memory: a consumer must be able to see "logged in, figures
// 22h old, newest refresh refused" without one fact overwriting another.
// Limits are present whenever any are known, and `usage.observed_at` says
// when — a consumer that ignores it is choosing to.
type jsonAccount struct {
	Vendor      string      `json:"vendor"` // "claude" | "codex"
	Name        string      `json:"name"`   // dir basename, or the primary's name
	Email       string      `json:"email,omitempty"`
	Launcher    string      `json:"launcher"`
	Plan        string      `json:"plan,omitempty"`
	Current     bool        `json:"current"`
	Health      string      `json:"health"`
	DirMismatch string      `json:"dir_mismatch,omitempty"`
	Usage       *jsonUsage  `json:"usage"`   // null = nothing known
	Attempt     jsonAttempt `json:"attempt"` // about the request, never the account
}

type jsonUsage struct {
	ObservedAt string `json:"observed_at"` // RFC3339 UTC
	Source     string `json:"source"`      // "live" | "headroom_cache" | "claude_cache" (never for Codex)
	Fresh      bool   `json:"fresh"`       // within the vendor's request spacing
	// Allowance is the account-level answer beside the windows: "blocked" is
	// positive evidence the vendor refuses work, whatever the percents say;
	// "unknown" (every Claude Code response) is never to be read as allowed.
	Allowance       string      `json:"allowance"` // "unknown" | "allowed" | "blocked" | "bad"
	AllowanceReason string      `json:"allowance_reason,omitempty"`
	BlockedFeatures []string    `json:"blocked_features,omitempty"` // limits blocked on their own; the account stays usable
	Limits          []jsonLimit `json:"limits"`
}

type jsonAttempt struct {
	State          string  `json:"state"`
	HTTPStatus     int     `json:"http_status,omitempty"`
	NextEligibleAt *string `json:"next_eligible_at,omitempty"` // RFC3339 UTC
}

// kind/group/model are the vendor's decoded identity vocabulary (see
// usage.Row) — what a machine consumer selects rows by. label stays what a
// human reads; matching it is matching prose, and prose moves when a model is
// renamed. identity_state is "bad" when the row could not be identified at
// all, so a consumer's empty match is distinguishable from a vanished limit.
type jsonLimit struct {
	Label         string  `json:"label"`
	Kind          string  `json:"kind,omitempty"`
	Group         string  `json:"group,omitempty"`
	Model         string  `json:"model,omitempty"`
	Feature       string  `json:"feature,omitempty"`        // Codex: metered_feature of an additional limit
	WindowSeconds int64   `json:"window_seconds,omitempty"` // Codex: the window's stated duration
	Percent       int     `json:"percent"`                  // 0 when percent_state is "bad"
	PercentState  string  `json:"percent_state"`
	ResetsAt      *string `json:"resets_at"` // RFC3339 UTC; null when unknown
	ResetState    string  `json:"reset_state"`
	Severity      string  `json:"severity"` // always "normal" for Codex
	IdentityState string  `json:"identity_state"`
	// Unstarted: nobody has spent against this window, so it has no reset yet
	// (reset_state "none", resets_at null). A fact about the row — the three
	// *_state fields keep their three values.
	Unstarted bool `json:"unstarted,omitempty"`
}

var healthNames = map[accountstate.Health]string{
	accountstate.HealthOK:              "ok",
	accountstate.HealthNoLogin:         "no_login",
	accountstate.HealthReloginRequired: "relogin_required",
	accountstate.HealthBadBlob:         "bad_blob",
	accountstate.HealthUnknown:         "unknown",
	accountstate.HealthUnprobed:        "unprobed", // the limits surface skipped the probe
}

var attemptNames = map[accountstate.AttemptState]string{
	accountstate.AttemptNone:                 "none",
	accountstate.AttemptPending:              "pending", // unreachable after a full drain
	accountstate.AttemptOK:                   "ok",
	accountstate.AttemptRefused:              "rate_limited",
	accountstate.AttemptDeferred:             "deferred",
	accountstate.AttemptTokenStale:           "access_token_stale",
	accountstate.AttemptCredentialUnreadable: "credential_unreadable",
	accountstate.AttemptTransport:            "transport_error",
	accountstate.AttemptHTTP:                 "http_error",
	accountstate.AttemptUnparseable:          "unparseable",
	accountstate.AttemptNoLimits:             "no_limits",
	accountstate.AttemptStateUnavailable:     "state_unavailable",
	accountstate.AttemptIdentityUnknown:      "identity_unknown",
}

// A consumer must be able to tell "this run asked the endpoint" from "a
// previous run asked and this one replayed the answer" — both are headroom's
// own reading, but only the first was made now. observed_at already carries
// the age; source carries who.
var sourceNames = map[accountstate.Source]string{
	accountstate.SourceLive:  "live",
	accountstate.SourceStore: "headroom_cache",
	accountstate.SourceCache: "claude_cache",
}

// vendorBoard is one vendor's share of a reporting surface: the scope, what
// was prepared under it, the current-target name those views were marked
// with, and the problems of that vendor's own state file.
type vendorBoard struct {
	scope    config.Scope
	st       *state.Store
	list     []*accountData
	current  string
	problems []state.Problem
}

func jsonDocument(boards []vendorBoard, generatedAt time.Time) ([]byte, error) {
	now := generatedAt.Unix()
	doc := jsonDoc{
		// 5: a second vendor. Every account and problem carries "vendor",
		// `current` is an object keyed by vendor, limits gain feature /
		// window_seconds / unstarted, and usage gains the allowance.
		Schema:      5,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Current:     map[string]string{},
		Accounts:    []jsonAccount{},
	}
	for _, b := range boards {
		doc.Current[string(b.scope.Vendor)] = b.current
		for _, p := range b.problems {
			doc.Problems = append(doc.Problems, jsonProblem{Vendor: string(b.scope.Vendor), Section: p.Section, Detail: p.Detail})
		}
		doc.appendAccounts(b, now)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func (doc *jsonDoc) appendAccounts(b vendorBoard, now int64) {
	list := b.list
	for _, d := range list {
		v := d.View
		a := jsonAccount{
			Vendor:      string(b.scope.Vendor),
			Name:        d.Acct.Name,
			Email:       d.Acct.Email,
			Launcher:    v.Launcher,
			Plan:        v.Plan,
			Current:     v.Current,
			Health:      healthNames[v.Health],
			DirMismatch: v.DirMismatch,
			Attempt: jsonAttempt{
				State: attemptNames[v.Attempt.State],
			},
		}
		if v.Attempt.State == accountstate.AttemptHTTP || v.Attempt.State == accountstate.AttemptRefused {
			a.Attempt.HTTPStatus = v.Attempt.HTTPCode
		}
		if v.Attempt.StoreError != "" {
			doc.Problems = append(doc.Problems, jsonProblem{Vendor: string(b.scope.Vendor), Section: "request[" + d.Acct.Name + "]", Detail: v.Attempt.StoreError})
		}
		if v.Attempt.State != accountstate.AttemptOK && v.Attempt.NextEligibleAt > now {
			ts := time.Unix(v.Attempt.NextEligibleAt, 0).UTC().Format(time.RFC3339)
			a.Attempt.NextEligibleAt = &ts
		}
		if v.Obs != nil {
			u := &jsonUsage{
				ObservedAt: time.Unix(v.Obs.ObservedAt, 0).UTC().Format(time.RFC3339),
				Source:     sourceNames[v.Obs.Source],
				Fresh:      v.Fresh(now),

				Allowance:       v.Obs.Allowance.State.Name(),
				AllowanceReason: v.Obs.Allowance.Reason,
				BlockedFeatures: v.Obs.Allowance.BlockedFeatures,
				Limits:          make([]jsonLimit, 0, len(v.Obs.Rows)),
			}
			for _, r := range v.Obs.Rows {
				l := jsonLimit{
					Label:         r.Label,
					Kind:          r.Kind,
					Group:         r.Group,
					Model:         r.Model,
					Feature:       r.Feature,
					WindowSeconds: r.WindowSeconds,
					Unstarted:     r.Unstarted,
					Percent:       r.Percent,
					PercentState:  r.PercentState.Name(),
					ResetState:    r.ResetState.Name(),
					Severity:      r.Severity,
					IdentityState: r.IdentityState.Name(),
				}
				if r.ResetAt != 0 {
					ts := time.Unix(r.ResetAt, 0).UTC().Format(time.RFC3339)
					l.ResetsAt = &ts
				}
				u.Limits = append(u.Limits, l)
			}
			a.Usage = u
		}
		doc.Accounts = append(doc.Accounts, a)
	}
}

// fetchBoards prepares every scope and runs one refresh round per scope, the
// rounds side by side: each owns its own list, store and channel, so the two
// vendors' results never meet.
func fetchBoards(scopes []config.Scope) []vendorBoard {
	boards := make([]vendorBoard, len(scopes))
	var wg sync.WaitGroup
	for i, scope := range scopes {
		wg.Add(1)
		go func(i int, scope config.Scope) {
			defer wg.Done()
			st := state.Open(scope)
			// current comes from prepare's snapshot: envelope and per-account
			// flags must agree even if a concurrent select rewrites .current
			// mid-fetch.
			list, current, snap := prepare(scope, st)
			for u := range launchFetches(context.Background(), list, st) {
				resolve(list[u.Index], u)
			}
			boards[i] = vendorBoard{scope: scope, st: st, list: list, current: current, problems: snap.Problems()}
		}(i, scope)
	}
	wg.Wait()
	return boards
}

func runDashboardJSON(scopes []config.Scope) int {
	data, err := jsonDocument(fetchBoards(scopes), time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
		return 1
	}
	os.Stdout.Write(append(data, '\n'))
	return 0
}
