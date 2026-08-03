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
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

type jsonDoc struct {
	Schema      int           `json:"schema"`
	GeneratedAt string        `json:"generated_at"` // RFC3339 UTC
	Current     string        `json:"current"`      // account name bare `x` targets
	Accounts    []jsonAccount `json:"accounts"`
}

// The account's three axes are three fields, for the same reason they are
// three fields in memory: a consumer must be able to see "logged in, figures
// 22h old, newest refresh refused" without one fact overwriting another.
// Limits are present whenever any are known, and `usage.observed_at` says
// when — a consumer that ignores it is choosing to.
type jsonAccount struct {
	Name        string      `json:"name"` // dir basename, or the primary's name
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
	ObservedAt string      `json:"observed_at"` // RFC3339 UTC
	Source     string      `json:"source"`      // "live" | "claude_cache"
	Fresh      bool        `json:"fresh"`       // within render.FreshWindow
	Limits     []jsonLimit `json:"limits"`
}

type jsonAttempt struct {
	State          string  `json:"state"`
	HTTPStatus     int     `json:"http_status,omitempty"`
	NextEligibleAt *string `json:"next_eligible_at,omitempty"` // RFC3339 UTC
}

type jsonLimit struct {
	Label        string  `json:"label"`
	Percent      int     `json:"percent"` // 0 when percent_state is "bad"
	PercentState string  `json:"percent_state"`
	ResetsAt     *string `json:"resets_at"` // RFC3339 UTC; null when unknown
	ResetState   string  `json:"reset_state"`
	Severity     string  `json:"severity"`
}

var healthNames = map[render.Health]string{
	render.HealthOK:              "ok",
	render.HealthNoLogin:         "no_login",
	render.HealthReloginRequired: "relogin_required",
	render.HealthBadBlob:         "bad_blob",
	render.HealthUnknown:         "unknown",
}

var attemptNames = map[render.AttemptState]string{
	render.AttemptNone:                 "none",
	render.AttemptPending:              "pending", // unreachable after a full drain
	render.AttemptOK:                   "ok",
	render.AttemptRefused:              "rate_limited",
	render.AttemptDeferred:             "deferred",
	render.AttemptTokenStale:           "access_token_stale",
	render.AttemptCredentialUnreadable: "credential_unreadable",
	render.AttemptTransport:            "transport_error",
	render.AttemptHTTP:                 "http_error",
	render.AttemptUnparseable:          "unparseable",
	render.AttemptNoLimits:             "no_limits",
	render.AttemptStateUnavailable:     "state_unavailable",
	render.AttemptIdentityUnknown:      "identity_unknown",
}

// A consumer must be able to tell "this run asked the endpoint" from "a
// previous run asked and this one replayed the answer" — both are headroom's
// own reading, but only the first was made now. observed_at already carries
// the age; source carries who.
var sourceNames = map[render.Source]string{
	render.SourceLive:  "live",
	render.SourceStore: "headroom_cache",
	render.SourceCache: "claude_cache",
}

func jsonDocument(list []*accountData, current string, generatedAt time.Time) ([]byte, error) {
	now := generatedAt.Unix()
	doc := jsonDoc{
		Schema:      3,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Current:     current,
		Accounts:    make([]jsonAccount, 0, len(list)),
	}
	for _, d := range list {
		v := d.View
		a := jsonAccount{
			Name:        d.Acct.Name,
			Email:       d.Acct.Email,
			Launcher:    v.Launcher,
			Plan:        v.Plan,
			Current:     v.Current,
			Health:      healthNames[v.Health],
			DirMismatch: v.DirMismatch,
			Attempt: jsonAttempt{
				State:      attemptNames[v.Attempt.State],
				HTTPStatus: v.Attempt.HTTPCode,
			},
		}
		if v.Attempt.NextEligibleAt > now {
			ts := time.Unix(v.Attempt.NextEligibleAt, 0).UTC().Format(time.RFC3339)
			a.Attempt.NextEligibleAt = &ts
		}
		if v.Obs != nil {
			u := &jsonUsage{
				ObservedAt: time.Unix(v.Obs.ObservedAt, 0).UTC().Format(time.RFC3339),
				Source:     sourceNames[v.Obs.Source],
				Fresh:      v.Fresh(now),
				Limits:     make([]jsonLimit, 0, len(v.Obs.Rows)),
			}
			for _, r := range v.Obs.Rows {
				l := jsonLimit{
					Label:        r.Label,
					Percent:      r.Percent,
					PercentState: r.PercentState.Name(),
					ResetState:   r.ResetState.Name(),
					Severity:     r.Severity,
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
	return json.MarshalIndent(doc, "", "  ")
}

func runDashboardJSON(cfg config.Config) int {
	// current comes from prepare's snapshot: envelope and per-account flags
	// must agree even if a concurrent select rewrites .current mid-fetch.
	st := state.Open(cfg.AccountsRoot)
	list, current := prepare(cfg, st)
	for u := range launchFetches(context.Background(), cfg, list, st) {
		resolve(list[u.idx], u, time.Now())
	}
	data, err := jsonDocument(list, current, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
		return 1
	}
	os.Stdout.Write(append(data, '\n'))
	return 0
}
