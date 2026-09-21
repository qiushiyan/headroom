package usage

import (
	"fmt"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// observedCodexBody is one real response with its identifiers replaced
// (codex-cli 0.155.0, 2026-09-21).
const observedCodexBody = `{"plan_type":"pro","account_id":"acct-1","user_id":"user-1","email":"a@x.com",
 "rate_limit":{"allowed":true,"limit_reached":false,
   "primary_window":{"used_percent":93,"limit_window_seconds":604800,
     "reset_after_seconds":259060,"reset_at":1790239759},
   "secondary_window":null},
 "code_review_rate_limit":null,
 "additional_rate_limits":null,
 "model_usage":{"gpt-6-astra":{"available":true,"available_at":null,"credits_would_enable":false}},
 "credits":{"has_credits":false,"unlimited":false,"overage_limit_reached":false,"balance":"0",
   "approx_local_messages":[0,0],"approx_cloud_messages":[0,0]},
 "spend_control":{"reached":false,"individual_limit":null},
 "rate_limit_reached_type":null,"promo":null,
 "rate_limit_reset_credits":{"available_count":1,"applicable_available_count":0}}`

func window(pct any, secs, after, at any) string {
	return fmt.Sprintf(`{"used_percent":%v,"limit_window_seconds":%v,"reset_after_seconds":%v,"reset_at":%v}`, pct, secs, after, at)
}

func codexBody(rateLimit, rest string) []byte {
	if rest != "" {
		rest = "," + rest
	}
	return []byte(`{"rate_limit":` + rateLimit + rest + `}`)
}

func mustCodex(t *testing.T, body []byte) Reading {
	t.Helper()
	r, err := Parse(config.Codex, body)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, body)
	}
	return r
}

func TestCodexObservedBody(t *testing.T) {
	r := mustCodex(t, []byte(observedCodexBody))
	if r.AccountID != "acct-1" || r.UserID != "user-1" || r.Plan != "pro" {
		t.Errorf("identity = %q %q %q", r.AccountID, r.UserID, r.Plan)
	}
	if r.Allowance.State != AllowanceAllowed || r.Allowance.Reason != "" || len(r.Allowance.BlockedFeatures) != 0 {
		t.Errorf("allowance = %+v", r.Allowance)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("rows = %+v", r.Rows)
	}
	want := Row{Label: "weekly", Kind: "primary", Group: "rate_limit", Percent: 93, ResetAt: 1790239759,
		Severity: "normal", WindowSeconds: 604800}
	if r.Rows[0] != want {
		t.Errorf("row = %+v\nwant  %+v", r.Rows[0], want)
	}
	if r.Drifted() != 0 {
		t.Error("the observed body reads as drifted")
	}
}

// The row table: slot, group, feature, duration — the vendor's own words —
// and the label derived from them alone.
func TestCodexRowsAndLabels(t *testing.T) {
	body := codexBody(
		`{"allowed":true,"limit_reached":false,"primary_window":`+window(10, 18000, 900, 1790000000)+
			`,"secondary_window":`+window(40, 604800, 9000, 1790500000)+`}`,
		`"code_review_rate_limit":{"allowed":true,"limit_reached":false,"primary_window":`+window(5, 604800, 100, 1790500000)+`},
		 "additional_rate_limits":[
		   {"limit_name":"Astra","metered_feature":"astra_model","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":`+window(7, 86400*3, 100, 1790100000)+`}},
		   {"limit_name":"","metered_feature":"cloud_tasks","rate_limit":{"primary_window":`+window(1, 5400, 100, 1790100000)+`}}]`)
	r := mustCodex(t, body)
	type ident struct {
		label, kind, group, feature string
		secs                        int64
	}
	want := []ident{
		{"5h", "primary", "rate_limit", "", 18000},
		{"weekly", "secondary", "rate_limit", "", 604800},
		{"weekly code review", "primary", "code_review_rate_limit", "", 604800},
		{"3d Astra", "primary", "additional", "astra_model", 259200},
		{"2h cloud_tasks", "primary", "additional", "cloud_tasks", 5400},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows = %+v", r.Rows)
	}
	for i, w := range want {
		g := r.Rows[i]
		if (ident{g.Label, g.Kind, g.Group, g.Feature, g.WindowSeconds}) != w {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
		if g.Model != "" || g.Severity != "normal" || g.Drifted() {
			t.Errorf("row %d: model %q severity %q drifted %v", i, g.Model, g.Severity, g.Drifted())
		}
	}
}

// Two windows that differ only in duration are different limits.
func TestCodexDurationIsIdentity(t *testing.T) {
	a := mustCodex(t, codexBody(`{"primary_window":`+window(1, 18000, 10, 1790000000)+`}`, "")).Rows[0]
	b := mustCodex(t, codexBody(`{"primary_window":`+window(1, 604800, 10, 1790000000)+`}`, "")).Rows[0]
	if a.Kind != b.Kind || a.Group != b.Group || a.WindowSeconds == b.WindowSeconds {
		t.Errorf("rows %+v / %+v", a, b)
	}
}

// The reset table. The three field states stay three; unstarted is a
// separate fact, and its ResetAt of 0 means it can never read as rolled over.
func TestCodexResetStates(t *testing.T) {
	for _, c := range []struct {
		name      string
		win       string
		state     FieldState
		unstarted bool
		resetAt   int64
	}{
		{"a parseable instant", window(10, 604800, 100, 1790000000), StateOK, false, 1790000000},
		{"the vendor states no reset", window(10, 604800, 100, "null"), StateNone, false, 0},
		{"present but unparseable", window(10, 604800, 100, `{"at":1}`), StateBad, false, 0},
		{"never used: a full window away", window(0, 604800, 604800, 1790604800), StateNone, true, 0},
		{"0% one second into a started window", window(0, 604800, 604799, 1790604799), StateOK, false, 1790604799},
		{"a full window away but something was spent", window(1, 604800, 604800, 1790604800), StateOK, false, 1790604800},
		{"unstarted shape with an undecodable remaining time", window(0, 604800, `"604800"`, 1790604800), StateOK, false, 1790604800},
	} {
		row := mustCodex(t, codexBody(`{"primary_window":`+c.win+`}`, "")).Rows[0]
		if row.ResetState != c.state || row.Unstarted != c.unstarted || row.ResetAt != c.resetAt {
			t.Errorf("%s: state %v unstarted %v resetAt %d", c.name, row.ResetState, row.Unstarted, row.ResetAt)
		}
		if c.unstarted && row.RolledOver(1<<40) {
			t.Errorf("%s: an unstarted row read as rolled over", c.name)
		}
	}
}

// Rejecting the envelope is separate from degrading a field.
func TestCodexEnvelopeAndDegradation(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `broken`, `{"plan_type":"pro"}`, `{"rate_limit":"soon"}`} {
		if _, err := Parse(config.Codex, []byte(body)); err == nil {
			t.Errorf("%s: parsed, want unparseable", body)
		}
	}
	// A null rate_limit is an observation of no limits.
	if r := mustCodex(t, codexBody(`null`, "")); len(r.Rows) != 0 || r.Allowance.State != AllowanceUnknown {
		t.Errorf("null rate_limit: %+v", r)
	}
	// Null windows, a null limit object and null additional limits add no rows.
	r := mustCodex(t, codexBody(`{"allowed":true,"limit_reached":false,"primary_window":null,"secondary_window":null}`,
		`"code_review_rate_limit":null,"additional_rate_limits":null`))
	if len(r.Rows) != 0 || r.Drifted() != 0 {
		t.Errorf("null windows: %+v", r.Rows)
	}

	for _, c := range []struct {
		name                     string
		win                      string
		percent, reset, identity FieldState
	}{
		{"a percent under a wrong type", window(`"93"`, 604800, 1, 1790000000), StateBad, StateOK, StateOK},
		{"a missing percent", `{"limit_window_seconds":604800,"reset_at":1790000000}`, StateBad, StateOK, StateOK},
		{"a non-positive duration", window(10, 0, 1, 1790000000), StateOK, StateOK, StateBad},
		{"a duration under a wrong type", window(10, `"604800"`, 1, 1790000000), StateOK, StateOK, StateBad},
		{"a window that is not an object", `7`, StateBad, StateNone, StateBad},
	} {
		rows := mustCodex(t, codexBody(`{"primary_window":`+c.win+`}`, "")).Rows
		if len(rows) != 1 {
			t.Fatalf("%s: rows = %+v", c.name, rows)
		}
		g := rows[0]
		if g.PercentState != c.percent || g.ResetState != c.reset || g.IdentityState != c.identity || !g.Drifted() {
			t.Errorf("%s: %+v", c.name, g)
		}
		if g.PercentState == StateBad && g.Percent != 0 {
			t.Errorf("%s: a bad percent kept a figure", c.name)
		}
	}

	// A malformed additional entry is one bad row — never dropped silently.
	for _, extra := range []string{`[7]`, `[{"limit_name":"x"}]`, `[{"metered_feature":"f","rate_limit":"x"}]`, `{"not":"a list"}`} {
		r := mustCodex(t, codexBody(`null`, `"additional_rate_limits":`+extra))
		if len(r.Rows) != 1 || r.Rows[0].IdentityState != StateBad || r.Rows[0].Group != CodexGroupAdditional {
			t.Errorf("additional %s: rows = %+v", extra, r.Rows)
		}
	}
}

// The allowance table, first match wins.
func TestCodexAllowance(t *testing.T) {
	rl := func(fields string) string { return `{` + fields + `}` }
	for _, c := range []struct {
		name   string
		body   []byte
		want   AllowanceState
		reason string
	}{
		{"allowed is false", codexBody(rl(`"allowed":false,"limit_reached":false`), ""), AllowanceBlocked, "rate_limit.allowed is false"},
		{"limit reached", codexBody(rl(`"allowed":true,"limit_reached":true`), ""), AllowanceBlocked, "rate_limit.limit_reached"},
		{"spend control reached", codexBody(rl(`"allowed":true,"limit_reached":false`), `"spend_control":{"reached":true}`), AllowanceBlocked, "spend_control.reached"},
		{"a reached type", codexBody(rl(`"allowed":true,"limit_reached":false`), `"rate_limit_reached_type":"workspace_owner_usage_limit_reached"`), AllowanceBlocked, "workspace_owner_usage_limit_reached"},
		{"blocked beside a malformed sibling", codexBody(rl(`"allowed":false,"limit_reached":"no"`), ""), AllowanceBlocked, "rate_limit.allowed is false"},
		{"blocked with low percents", codexBody(rl(`"allowed":false,"limit_reached":false,"primary_window":`+window(3, 604800, 5, 1790000000)), ""), AllowanceBlocked, "rate_limit.allowed is false"},
		{"a malformed field and nothing positive", codexBody(rl(`"allowed":true,"limit_reached":"no"`), ""), AllowanceBad, ""},
		{"a malformed reached type", codexBody(rl(`"allowed":true,"limit_reached":false`), `"rate_limit_reached_type":7`), AllowanceBad, ""},
		{"an empty reached type is malformed, neither a block nor allowed", codexBody(rl(`"allowed":true,"limit_reached":false`), `"rate_limit_reached_type":""`), AllowanceBad, ""},
		{"a null boolean is absence, as a null window is", codexBody(rl(`"allowed":true,"limit_reached":false`), `"spend_control":{"reached":null}`), AllowanceAllowed, ""},
		{"a malformed spend control", codexBody(rl(`"allowed":true,"limit_reached":false`), `"spend_control":"x"`), AllowanceBad, ""},
		{"allowed", codexBody(rl(`"allowed":true,"limit_reached":false`), `"spend_control":{"reached":false},"rate_limit_reached_type":null`), AllowanceAllowed, ""},
		{"allowed alone says too little", codexBody(rl(`"allowed":true`), ""), AllowanceUnknown, ""},
		{"nothing said", codexBody(`null`, ""), AllowanceUnknown, ""},
	} {
		r := mustCodex(t, c.body)
		if r.Allowance.State != c.want || r.Allowance.Reason != c.reason {
			t.Errorf("%s: allowance = %+v, want %v %q", c.name, r.Allowance, c.want, c.reason)
		}
		if (c.want == AllowanceBad) != (r.Drifted() > 0) {
			t.Errorf("%s: drifted = %d", c.name, r.Drifted())
		}
	}
	// Every Claude Code response is unknown — and unknown never blocks.
	if r, err := Parse(config.Claude, []byte(`{"limits":[]}`)); err != nil || r.Allowance.State != AllowanceUnknown {
		t.Errorf("claude reading = %+v %v", r, err)
	}
}

// A block on one feature is named and never changes the account state — even
// when the entry has no windows at all.
func TestCodexFeatureOnlyBlocks(t *testing.T) {
	r := mustCodex(t, codexBody(`{"allowed":true,"limit_reached":false}`,
		`"code_review_rate_limit":{"allowed":false,"limit_reached":true},
		 "additional_rate_limits":[{"limit_name":"Astra","metered_feature":"astra_model","rate_limit":{"allowed":true,"limit_reached":true}},
		                           {"limit_name":"","metered_feature":"cloud_tasks","rate_limit":{"allowed":false}},
		                           {"limit_name":"Fine","metered_feature":"fine","rate_limit":{"allowed":true,"limit_reached":false}}]`))
	if r.Allowance.State != AllowanceAllowed {
		t.Errorf("a feature block changed the account state: %+v", r.Allowance)
	}
	want := []string{"code review", "Astra", "cloud_tasks"}
	if fmt.Sprint(r.Allowance.BlockedFeatures) != fmt.Sprint(want) {
		t.Errorf("blocked features = %v, want %v", r.Allowance.BlockedFeatures, want)
	}
	if len(r.Rows) != 0 {
		t.Errorf("rows = %+v", r.Rows)
	}
}

// One dispatch: the vendor decides the parser, and neither reads the other's
// document as its own.
func TestParseDispatch(t *testing.T) {
	claude := []byte(`{"limits":[{"kind":"session","percent":5}]}`)
	if r, err := Parse(config.Claude, claude); err != nil || len(r.Rows) != 1 || r.Rows[0].Kind != "session" {
		t.Errorf("claude: %+v %v", r, err)
	}
	if _, err := Parse(config.Codex, claude); err == nil {
		t.Error("a Claude Code body parsed as Codex")
	}
	if _, err := Parse(config.Claude, []byte(observedCodexBody)); err == nil {
		t.Error("a Codex body parsed as Claude Code")
	}
}

// The unstarted predicate is about what was spent, not about what the percent
// rounds to: a sliver of use starts the window, and its reset is real.
func TestCodexUnstartedTestsTheDecodedPercentNotTheRoundedOne(t *testing.T) {
	row := mustCodex(t, codexBody(`{"primary_window":`+window(0.1, 604800, 604800, 1790604800)+`}`, "")).Rows[0]
	if row.Unstarted || row.ResetAt != 1790604800 || row.ResetState != StateOK {
		t.Errorf("0.1%% used read as not started and lost its reset: %+v", row)
	}
	if row.Percent != 0 {
		t.Errorf("display percent = %d", row.Percent)
	}
}
