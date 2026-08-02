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
	"github.com/qiushiyan/headroom/internal/usage"
)

type jsonDoc struct {
	Schema      int           `json:"schema"`
	GeneratedAt string        `json:"generated_at"` // RFC3339 UTC
	Current     string        `json:"current"`      // account name bare `x` targets
	Accounts    []jsonAccount `json:"accounts"`
}

type jsonAccount struct {
	Name        string      `json:"name"` // dir basename, or the primary's name
	Email       string      `json:"email,omitempty"`
	Launcher    string      `json:"launcher"`
	Plan        string      `json:"plan,omitempty"`
	Current     bool        `json:"current"`
	Status      string      `json:"status"`
	HTTPStatus  int         `json:"http_status,omitempty"`
	DirMismatch string      `json:"dir_mismatch,omitempty"`
	Limits      []jsonLimit `json:"limits,omitempty"`
}

type jsonLimit struct {
	Label        string  `json:"label"`
	Percent      int     `json:"percent"` // 0 when percent_state is "bad"
	PercentState string  `json:"percent_state"`
	ResetsAt     *string `json:"resets_at"` // RFC3339 UTC; null when unknown
	ResetState   string  `json:"reset_state"`
	Severity     string  `json:"severity"`
}

var statusNames = map[render.Status]string{
	render.StatusRows:        "ok",
	render.StatusPending:     "pending", // unreachable after a full drain
	render.StatusNoLogin:     "no_login",
	render.StatusBadBlob:     "bad_blob",
	render.StatusExpired:     "expired",
	render.StatusFetchFailed: "fetch_failed",
	render.StatusHTTPError:   "http_error",
	render.StatusUnparseable: "unparseable",
	render.StatusNoLimits:    "no_limits",
}

var stateNames = map[usage.FieldState]string{
	usage.StateOK:   "ok",
	usage.StateNone: "none",
	usage.StateBad:  "bad",
}

func jsonDocument(list []*accountData, current string, generatedAt time.Time) ([]byte, error) {
	doc := jsonDoc{
		Schema:      1,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Current:     current,
		Accounts:    make([]jsonAccount, 0, len(list)),
	}
	for _, d := range list {
		a := jsonAccount{
			Name:        d.Acct.Name,
			Email:       d.Acct.Email,
			Launcher:    d.View.Launcher,
			Plan:        d.View.Plan,
			Current:     d.View.Current,
			Status:      statusNames[d.View.Status],
			HTTPStatus:  d.View.HTTPCode,
			DirMismatch: d.View.DirMismatch,
		}
		for _, r := range d.View.Rows {
			l := jsonLimit{
				Label:        r.Label,
				Percent:      r.Percent,
				PercentState: stateNames[r.PercentState],
				ResetState:   stateNames[r.ResetState],
				Severity:     r.Severity,
			}
			if r.ResetAt != 0 {
				ts := time.Unix(r.ResetAt, 0).UTC().Format(time.RFC3339)
				l.ResetsAt = &ts
			}
			a.Limits = append(a.Limits, l)
		}
		doc.Accounts = append(doc.Accounts, a)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func runDashboardJSON(cfg config.Config) int {
	// current comes from prepare's snapshot: envelope and per-account flags
	// must agree even if a concurrent select rewrites .current mid-fetch.
	list, current := prepare(cfg)
	for u := range launchFetches(context.Background(), cfg, list) {
		resolve(list[u.idx], u.res)
	}
	data, err := jsonDocument(list, current, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom: %v\n", err)
		return 1
	}
	os.Stdout.Write(append(data, '\n'))
	return 0
}
