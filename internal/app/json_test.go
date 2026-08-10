package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/usage"
)

func TestJSONDocument(t *testing.T) {
	generatedAt := time.Unix(1754121000, 0)
	list := []*accountData{
		{
			Acct: accounts.Account{Name: "primary", Email: "p@x.com"},
			View: render.AccountView{
				Label: "p@x.com", Launcher: "x-primary", Plan: "max 20x", Current: true,
				Health: render.HealthOK,
				Obs: &render.Observation{
					ObservedAt: generatedAt.Unix() - 10,
					Source:     render.SourceLive,
					Rows: []usage.Row{
						{Label: "5h session", Kind: "session", Group: "session",
							Percent: 42, ResetAt: 1754121600,
							Severity: "normal", PercentState: usage.StateOK, ResetState: usage.StateOK},
						{Label: "All models (7d)", Kind: "weekly_all", Group: "weekly", Severity: "normal",
							PercentState: usage.StateBad, ResetState: usage.StateNone},
						{Label: "?", Percent: 81, Severity: "normal",
							PercentState: usage.StateOK, ResetState: usage.StateNone,
							IdentityState: usage.StateBad},
						{Label: "Fable (7d)", Kind: "weekly_scoped", Group: "weekly", Model: "Fable",
							Percent: 92, Severity: "critical",
							PercentState: usage.StateOK, ResetState: usage.StateNone},
					},
				},
				Attempt: render.Attempt{State: render.AttemptOK},
			},
		},
		{
			Acct: accounts.Account{ConfigDir: "/x", Name: "b@x.com"},
			View: render.AccountView{Label: "b@x.com", Launcher: "x-b", Health: render.HealthOK,
				Attempt: render.Attempt{State: render.AttemptHTTP, HTTPCode: 401}},
		},
	}
	data, err := jsonDocument(list, "primary", nil, generatedAt)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if doc["schema"] != float64(4) || doc["current"] != "primary" {
		t.Errorf("envelope: %v %v", doc["schema"], doc["current"])
	}
	if doc["generated_at"] != "2025-08-02T07:50:00Z" {
		t.Errorf("generated_at not UTC RFC3339: %v", doc["generated_at"])
	}

	accts := doc["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("got %d accounts", len(accts))
	}
	a0 := accts[0].(map[string]any)
	if a0["health"] != "ok" || a0["current"] != true || a0["launcher"] != "x-primary" {
		t.Errorf("account 0: %v", a0)
	}
	u0 := a0["usage"].(map[string]any)
	if u0["source"] != "live" || u0["fresh"] != true || u0["observed_at"] != "2025-08-02T07:49:50Z" {
		t.Errorf("usage provenance: %v", u0)
	}
	limits := u0["limits"].([]any)
	l0 := limits[0].(map[string]any)
	if l0["percent"] != float64(42) || l0["percent_state"] != "ok" ||
		l0["resets_at"] != "2025-08-02T08:00:00Z" || l0["reset_state"] != "ok" {
		t.Errorf("limit 0: %v", l0)
	}
	// Identity travels decoded, beside the label — a consumer selects by
	// kind/group/model equality, never by matching the rendered prose.
	if l0["kind"] != "session" || l0["group"] != "session" || l0["identity_state"] != "ok" {
		t.Errorf("limit 0 identity: %v", l0)
	}
	if _, present := l0["model"]; present {
		t.Errorf("unscoped row should omit model: %v", l0)
	}
	// Drift stays visible: zero percent is accompanied by an explicit "bad",
	// and an absent timestamp is null with state "none", not invented.
	l1 := limits[1].(map[string]any)
	if l1["percent"] != float64(0) || l1["percent_state"] != "bad" {
		t.Errorf("bad percent not tagged: %v", l1)
	}
	if v, present := l1["resets_at"]; !present || v != nil {
		t.Errorf("absent reset should be null: %v", l1)
	}
	if l1["reset_state"] != "none" {
		t.Errorf("reset state: %v", l1)
	}
	// An unidentifiable row says so, and a scoped row carries its model.
	l2 := limits[2].(map[string]any)
	if l2["identity_state"] != "bad" {
		t.Errorf("unidentifiable row not tagged: %v", l2)
	}
	if _, present := l2["kind"]; present {
		t.Errorf("absent kind should be omitted: %v", l2)
	}
	l3 := limits[3].(map[string]any)
	if l3["kind"] != "weekly_scoped" || l3["model"] != "Fable" || l3["identity_state"] != "ok" {
		t.Errorf("scoped row identity: %v", l3)
	}

	a1 := accts[1].(map[string]any)
	if a1["health"] != "ok" {
		t.Errorf("a failed request must not change health: %v", a1["health"])
	}
	at1 := a1["attempt"].(map[string]any)
	if at1["state"] != "http_error" || at1["http_status"] != float64(401) {
		t.Errorf("account 1 attempt: %v", at1)
	}
	if v, present := a1["usage"]; !present || v != nil {
		t.Errorf("account with nothing known should carry usage:null: %v", a1)
	}
	if _, present := a1["email"]; present {
		t.Errorf("empty email should be omitted: %v", a1)
	}
}

// A consumer must be able to read "these figures are old, and the refresh
// that would have replaced them was refused" as two separate facts. Schema 1
// forced a choice between the limits and the error.
func TestJSONSeparatesStaleDataFromFailedRefresh(t *testing.T) {
	generatedAt := time.Unix(1754121000, 0)
	list := []*accountData{{
		Acct: accounts.Account{Name: "a@x.com"},
		View: render.AccountView{
			Launcher: "x-a", Health: render.HealthOK,
			Obs: &render.Observation{
				ObservedAt: generatedAt.Add(-22 * time.Hour).Unix(),
				Source:     render.SourceCache,
				Rows:       []usage.Row{{Label: "5h session", Percent: 58, Severity: "normal"}},
			},
			Attempt: render.Attempt{
				State: render.AttemptRefused, HTTPCode: 429,
				NextEligibleAt: generatedAt.Add(3 * time.Minute).Unix(),
			},
		},
	}}
	data, err := jsonDocument(list, "a@x.com", nil, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	a := doc["accounts"].([]any)[0].(map[string]any)

	if a["health"] != "ok" {
		t.Errorf("rate limiting leaked into health: %v", a["health"])
	}
	u := a["usage"].(map[string]any)
	if u["source"] != "claude_cache" {
		t.Errorf("source not exposed: %v", u["source"])
	}
	if u["fresh"] != false {
		t.Error("22h-old figures reported as fresh")
	}
	if len(u["limits"].([]any)) != 1 {
		t.Error("known figures dropped because the refresh failed")
	}
	at := a["attempt"].(map[string]any)
	if at["state"] != "rate_limited" || at["next_eligible_at"] != "2025-08-02T07:53:00Z" {
		t.Errorf("attempt: %v", at)
	}
}

// Every attempt state a run can actually produce needs a wire name. A missing
// map entry serialises as "", which is worse than a wrong value: a consumer
// can't tell it from an absent field, and the schema is a versioned contract.
func TestEveryAttemptStateHasAWireName(t *testing.T) {
	for s := render.AttemptNone; s <= render.AttemptNoLimits; s++ {
		if attemptNames[s] == "" {
			t.Errorf("attempt state %d has no wire name", s)
		}
	}
	for h := render.HealthOK; h <= render.HealthUnprobed; h++ {
		if healthNames[h] == "" {
			t.Errorf("health state %d has no wire name", h)
		}
	}
}
