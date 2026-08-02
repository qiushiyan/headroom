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
	list := []*accountData{
		{
			Acct: accounts.Account{Name: "primary", Email: "p@x.com"},
			View: render.AccountView{
				Label: "p@x.com", Launcher: "x-primary", Plan: "max 20x", Current: true,
				Status: render.StatusRows,
				Rows: []usage.Row{
					{Label: "5h session", Percent: 42, ResetAt: 1754121600,
						Severity: "normal", PercentState: usage.StateOK, ResetState: usage.StateOK},
					{Label: "All models (7d)", Severity: "normal",
						PercentState: usage.StateBad, ResetState: usage.StateNone},
				},
			},
		},
		{
			Acct: accounts.Account{ConfigDir: "/x", Name: "b@x.com"},
			View: render.AccountView{Label: "b@x.com", Launcher: "x-b",
				Status: render.StatusHTTPError, HTTPCode: 401},
		},
	}
	data, err := jsonDocument(list, "primary", time.Unix(1754121000, 0))
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if doc["schema"] != float64(1) || doc["current"] != "primary" {
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
	if a0["status"] != "ok" || a0["current"] != true || a0["launcher"] != "x-primary" {
		t.Errorf("account 0: %v", a0)
	}
	limits := a0["limits"].([]any)
	l0 := limits[0].(map[string]any)
	if l0["percent"] != float64(42) || l0["percent_state"] != "ok" ||
		l0["resets_at"] != "2025-08-02T08:00:00Z" || l0["reset_state"] != "ok" {
		t.Errorf("limit 0: %v", l0)
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

	a1 := accts[1].(map[string]any)
	if a1["status"] != "http_error" || a1["http_status"] != float64(401) {
		t.Errorf("account 1: %v", a1)
	}
	if _, present := a1["limits"]; present {
		t.Errorf("no-rows account should omit limits: %v", a1)
	}
	if _, present := a1["email"]; present {
		t.Errorf("empty email should be omitted: %v", a1)
	}
}
