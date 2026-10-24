package app

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

func TestJSONDocument(t *testing.T) {
	generatedAt := time.Unix(1754121000, 0)
	list := []*accountData{
		{
			Acct: accounts.Account{Name: "primary", Email: "p@x.com"},
			View: accountstate.Facts{
				Label: "p@x.com", Launcher: "x-primary", Plan: "max 20x", Current: true,
				Health: accountstate.HealthOK,
				Obs: &accountstate.Observation{
					ObservedAt: generatedAt.Unix() - 10,
					Source:     accountstate.SourceLive,
					Rows: []usage.Row{
						{Label: "5h session", Kind: "session", Group: "session",
							Percent: 42, ResetAt: 1754121600,
							Severity: "normal", PercentState: tag.OK, ResetState: tag.OK},
						{Label: "All models (7d)", Kind: "weekly_all", Group: "weekly", Severity: "normal",
							PercentState: tag.Bad, ResetState: tag.None},
						{Label: "?", Percent: 81, Severity: "normal",
							PercentState: tag.OK, ResetState: tag.None,
							IdentityState: tag.Bad},
						{Label: "Fable (7d)", Kind: "weekly_scoped", Group: "weekly", Model: "Fable",
							Percent: 92, Severity: "critical",
							PercentState: tag.OK, ResetState: tag.None},
					},
				},
				// Its next request's spacing: an answer that just landed names no retry.
				Attempt: accountstate.Attempt{State: accountstate.AttemptOK, NextEligibleAt: generatedAt.Unix() + 90},
			},
		},
		{
			Acct: accounts.Account{ConfigDir: "/x", Name: "b@x.com"},
			View: accountstate.Facts{Label: "b@x.com", Launcher: "x-b", Health: accountstate.HealthOK,
				Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}},
		},
		// "These figures are old, and the refresh that would have replaced
		// them was refused — and the refusal could not be recorded" is three
		// facts. Schema 1 forced a choice between the limits and the error.
		{
			Acct: accounts.Account{Name: "a@x.com"},
			View: accountstate.Facts{
				Launcher: "x-a", Health: accountstate.HealthOK,
				Obs: &accountstate.Observation{
					ObservedAt: generatedAt.Add(-22 * time.Hour).Unix(),
					Source:     accountstate.SourceCache,
					Rows:       []usage.Row{{Label: "5h session", Percent: 58, Severity: "normal"}},
				},
				Attempt: accountstate.Attempt{
					State: accountstate.AttemptRefused, HTTPCode: 429,
					NextEligibleAt: generatedAt.Add(3 * time.Minute).Unix(),
					StoreError:     "state file unavailable",
				},
			},
		},
		// A 200 reporting no limit windows is not "ok", and does name when the
		// account can next be asked.
		{
			Acct: accounts.Account{Name: "n@x.com"},
			View: accountstate.Facts{
				Launcher: "x-n", Health: accountstate.HealthOK,
				Obs:     &accountstate.Observation{ObservedAt: generatedAt.Unix(), Source: accountstate.SourceLive},
				Attempt: accountstate.Attempt{State: accountstate.AttemptNoLimits, NextEligibleAt: generatedAt.Unix() + 90},
			},
		},
	}
	data, err := jsonDocument(claudeBoard(list, "primary"), generatedAt)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if doc["schema"] != float64(7) || doc["current"].(map[string]any)["claude"] != "primary" {
		t.Errorf("envelope: %v %v", doc["schema"], doc["current"])
	}
	if doc["generated_at"] != "2025-08-02T07:50:00Z" {
		t.Errorf("generated_at not UTC RFC3339: %v", doc["generated_at"])
	}

	accts := doc["accounts"].([]any)
	if len(accts) != 4 {
		t.Fatalf("got %d accounts", len(accts))
	}
	a0 := accts[0].(map[string]any)
	if a0["health"] != "ok" || a0["current"] != true || a0["launcher"] != "x-primary" {
		t.Errorf("account 0: %v", a0)
	}
	if at0 := a0["attempt"].(map[string]any); at0["state"] != "ok" || at0["next_eligible_at"] != nil || at0["http_status"] != nil {
		t.Errorf("a successful attempt gained error or retry fields: %v", at0)
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

	a2 := accts[2].(map[string]any)
	if a2["health"] != "ok" {
		t.Errorf("rate limiting leaked into health: %v", a2["health"])
	}
	u2 := a2["usage"].(map[string]any)
	if u2["source"] != "claude_cache" || u2["fresh"] != false || len(u2["limits"].([]any)) != 1 {
		t.Errorf("22h-old cached figures: %v — known figures must survive a failed refresh, marked not fresh", u2)
	}
	at2 := a2["attempt"].(map[string]any)
	if at2["state"] != "rate_limited" || at2["http_status"] != float64(429) || at2["next_eligible_at"] != "2025-08-02T07:53:00Z" {
		t.Errorf("account 2 attempt: %v", at2)
	}
	// The bookkeeping failure is headroom's own problem, filed under the
	// request it belongs to, beside the endpoint's verdict above.
	var problems []string
	listed, _ := doc["problems"].([]any)
	for _, p := range listed {
		p := p.(map[string]any)
		problems = append(problems, p["vendor"].(string)+":"+p["section"].(string))
	}
	if len(problems) != 1 || problems[0] != "claude:request[a@x.com]" {
		t.Errorf("problems = %v, want the one request's bookkeeping under its vendor", problems)
	}

	if at3 := accts[3].(map[string]any)["attempt"].(map[string]any); at3["state"] != "no_limits" || at3["next_eligible_at"] != "2025-08-02T07:51:30Z" {
		t.Errorf("account 3 attempt: %v", at3)
	}
}

// Every attempt state, health and source needs a wire name. A missing map
// entry serialises as "", which is worse than a wrong value: a consumer can't
// tell it from an absent field, and the schema is a versioned contract. The
// states are counted from their declarations, not up to a named constant: a
// loop bounded by one stops being complete the day a state is appended after
// it, which is how state_unavailable and identity_unknown went unchecked.
func TestEveryAttemptStateHasAWireName(t *testing.T) {
	for i := range declared(t, "AttemptState") {
		if attemptNames[accountstate.AttemptState(i)] == "" {
			t.Errorf("attempt state %d has no wire name", i)
		}
	}
	for i := range declared(t, "Health") {
		if healthNames[accountstate.Health(i)] == "" {
			t.Errorf("health state %d has no wire name", i)
		}
	}
	for i := range declared(t, "Source") {
		if sourceNames[accountstate.Source(i)] == "" {
			t.Errorf("source %d has no wire name", i)
		}
	}
}

// declared counts the constants of the iota block that declares typ in
// accountstate's facts.go, read from the source.
func declared(t *testing.T, typ string) int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "accountstate", "facts.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		if id, ok := g.Specs[0].(*ast.ValueSpec).Type.(*ast.Ident); !ok || id.Name != typ {
			continue
		}
		n := 0
		for _, s := range g.Specs {
			n += len(s.(*ast.ValueSpec).Names)
		}
		return n
	}
	t.Fatalf("facts.go declares no %s constants", typ)
	return 0
}
