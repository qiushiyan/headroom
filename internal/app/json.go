package app

// The dashboard for machines: same pipeline, same tagged degradation,
// serialized instead of drawn. Consumers get the drift tags as explicit
// state strings — a bad field is distinguishable from a zero or an absent
// one, exactly as in the human view. Field changes bump "schema".

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

type jsonDoc struct {
	Schema      int    `json:"schema"`
	GeneratedAt string `json:"generated_at"` // RFC3339 UTC
	// Current is keyed by vendor and holds only the vendors in this document:
	// the account name a bare launch of that vendor targets ("" = none
	// resolvable). The same email can be an account of both vendors, so one
	// string could not say which.
	Current map[string]string `json:"current"`
	// Mode is keyed by vendor like Current: what a bare launch of that vendor
	// does — "pinned" to current's account, "auto" (it chooses the
	// least-loaded account, and current is then ""), or "" when `.current`
	// cannot be resolved. A consumer that read an empty current as corrupt
	// routing state must read mode first.
	Mode     map[string]string `json:"mode"`
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

	// Load is what an automatic launch counts on the account beyond its
	// figures: busy sessions and recent launches, from every home sharing
	// the ledger, split by home. null when the surface did not read it —
	// `limits` never does, since it spawns nothing.
	Load *jsonLoad `json:"load"`
}

type jsonLoad struct {
	Value    int            `json:"value"`    // what an automatic launch ranks by: session usage in steps of ten, plus busy, plus launched
	Busy     int            `json:"busy"`     // verified-live sessions the vendor reports as working
	Launched int            `json:"launched"` // launches of the last fifteen minutes not yet among them
	Homes    []jsonHomeLoad `json:"homes"`    // whose they are; empty when there are none
}

type jsonHomeLoad struct {
	Home     string `json:"home"`  // the home's accounts root
	Label    string `json:"label"` // what a person calls it
	This     bool   `json:"this"`  // the home this document was made in
	Busy     int    `json:"busy"`
	Launched int    `json:"launched"`
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

// jsonDocument serializes one board per vendor. Each vendor's `current` and
// `mode` are its board's routing — the read of `.current` that marked the
// rows — so the envelope and the per-account flags say one thing even if the
// file is rewritten while a round is in flight.
func jsonDocument(boards []round.View, generatedAt time.Time) ([]byte, error) {
	now := generatedAt.Unix()
	doc := jsonDoc{
		// 7: `load` on every account — busy sessions and recent launches,
		// split by the home they came from, since a second home on the machine
		// may hold logins of the same subscriptions and share the ledger.
		// 6: automatic placement. `mode` is keyed by vendor, and under "auto"
		// that vendor's `current` is "" with no account marked current — an
		// empty current no longer means corrupt routing state on its own.
		// (5: a second vendor — "vendor" on every account and problem,
		// `current` keyed by vendor, feature / window_seconds / unstarted on
		// limits, the allowance on usage.)
		Schema:      7,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Current:     map[string]string{},
		Mode:        map[string]string{},
		Accounts:    []jsonAccount{},
	}
	for _, b := range boards {
		doc.Current[string(b.Scope.Vendor)] = b.Routing.Current
		doc.Mode[string(b.Scope.Vendor)] = b.Routing.Mode
		for _, p := range b.Problems {
			doc.Problems = append(doc.Problems, jsonProblem{Vendor: string(b.Scope.Vendor), Section: p.Section, Detail: p.Detail})
		}
		doc.appendAccounts(b, now)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func (doc *jsonDoc) appendAccounts(b round.View, now int64) {
	for _, d := range b.Accounts {
		v := d.View
		a := jsonAccount{
			Vendor:      string(b.Scope.Vendor),
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
			doc.Problems = append(doc.Problems, jsonProblem{Vendor: string(b.Scope.Vendor), Section: "request[" + d.Acct.Name + "]", Detail: v.Attempt.StoreError})
		}
		if v.Attempt.State != accountstate.AttemptOK && v.Attempt.NextEligibleAt > now {
			ts := time.Unix(v.Attempt.NextEligibleAt, 0).UTC().Format(time.RFC3339)
			a.Attempt.NextEligibleAt = &ts
		}
		if v.Load != nil {
			l := &jsonLoad{Value: v.Load.Value, Busy: v.Load.Busy, Launched: v.Load.Launched, Homes: []jsonHomeLoad{}}
			for _, h := range v.Load.Homes {
				l.Homes = append(l.Homes, jsonHomeLoad{Home: h.Home, Label: h.Label, This: h.This, Busy: h.Busy, Launched: h.Launched})
			}
			a.Load = l
		}
		if v.Obs != nil {
			u := &jsonUsage{
				ObservedAt: time.Unix(v.Obs.ObservedAt, 0).UTC().Format(time.RFC3339),
				Source:     v.Obs.Source.Name(),
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

// fetchBoards runs one full round per scope — prepare, the claim and whatever
// fetches it permits, then the marks — the rounds side by side: each owns its
// own rows and channel, so the two vendors' results never meet.
func fetchBoards(scopes []config.Scope, deps BoardDeps) []round.View {
	boards := make([]round.View, len(scopes))
	var wg sync.WaitGroup
	for i, scope := range scopes {
		wg.Go(func() {
			r := round.Prepare(context.Background(), scope, state.Open(scope), time.Now(), deps.Round)
			r.Complete(context.Background())
			markBoard(r, time.Now(), deps.Place)
			boards[i] = r.View()
		})
	}
	wg.Wait()
	return boards
}

// BoardJSON is the board as a versioned document: one full round per vendor
// — probes and, budget permitting, fetches — for scripts that want a
// refresh.
func BoardJSON(std Streams, scopes []config.Scope, deps BoardDeps) error {
	data, err := jsonDocument(fetchBoards(scopes, deps), time.Now())
	if err != nil {
		return err
	}
	_, err = std.Out.Write(append(data, '\n'))
	return err
}
