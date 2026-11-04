package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
)

type doc5 struct {
	Schema   int `json:"schema"`
	Accounts []struct {
		Vendor string `json:"vendor"`
		Name   string `json:"name"`
		Plan   string `json:"plan"`
		Usage  *struct {
			Source          string   `json:"source"`
			Allowance       string   `json:"allowance"`
			AllowanceReason string   `json:"allowance_reason"`
			BlockedFeatures []string `json:"blocked_features"`
			Limits          []struct {
				Kind          string  `json:"kind"`
				Group         string  `json:"group"`
				Feature       string  `json:"feature"`
				WindowSeconds int64   `json:"window_seconds"`
				Unstarted     bool    `json:"unstarted"`
				ResetsAt      *string `json:"resets_at"`
				PercentState  string  `json:"percent_state"`
				ResetState    string  `json:"reset_state"`
				IdentityState string  `json:"identity_state"`
				Severity      string  `json:"severity"`
			} `json:"limits"`
		} `json:"usage"`
	} `json:"accounts"`
	Problems []struct {
		Vendor  string `json:"vendor"`
		Section string `json:"section"`
	} `json:"problems"`
}

func decode5(t *testing.T, data []byte) doc5 {
	t.Helper()
	var d doc5
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, data)
	}
	// No routing fields, on the envelope or any account: every bare launch
	// places itself (schema 8).
	var envelope map[string]json.RawMessage
	var accts []map[string]json.RawMessage
	json.Unmarshal(data, &envelope)
	json.Unmarshal(envelope["accounts"], &accts)
	for _, field := range []string{"current", "mode"} {
		if _, ok := envelope[field]; ok {
			t.Fatalf("the document carries %q", field)
		}
	}
	for _, a := range accts {
		if _, ok := a["current"]; ok {
			t.Fatalf("an account carries \"current\": %v", a)
		}
	}
	return d
}

// Obligation 15: the schema 5 contract over a two-vendor fixture, from the
// fetching surface and from `limits`, unfiltered and filtered.
func TestSchema5TwoVendors(t *testing.T) {
	// The response names another plan than the login's id token does, so the
	// test can tell which one a surface reported.
	body := `{"plan_type":"prolite","account_id":"acct-1","user_id":"user-1",
	  "rate_limit":{"allowed":false,"limit_reached":true,
	    "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000,"reset_at":1790018000},
	    "secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":5000,"reset_at":1790239759}},
	  "code_review_rate_limit":{"allowed":false,"limit_reached":false},
	  "additional_rate_limits":[{"limit_name":"Astra","metered_feature":"astra_model","rate_limit":{"allowed":true,"limit_reached":false,
	    "primary_window":{"used_percent":7,"limit_window_seconds":86400,"reset_after_seconds":100,"reset_at":1790080000}}}],
	  "spend_control":{"reached":false},"rate_limit_reached_type":{"type":"usage_limit_reached"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	f.extra(t, login(1))
	// The same email as an account of both vendors, and one only Claude Code has.
	for _, name := range []string{"u1@x.com", "claude-only@x.com"} {
		if err := os.MkdirAll(filepath.Join(f.cfg.Claude.AccountsRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir()) // no claude, no security: probes answer "unavailable"
	both := []config.Scope{f.cfg.Claude, f.scope}

	data, err := jsonDocument(fetchBoards(both, BoardDeps{Round: LiveRound(true), Place: quietPlace()}), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := decode5(t, data)
	if d.Schema != 8 {
		t.Errorf("schema = %d", d.Schema)
	}
	vendors := map[string]int{}
	for _, a := range d.Accounts {
		vendors[a.Vendor]++
		if a.Vendor != "codex" || a.Name != "u1@x.com" {
			if a.Vendor == "claude" && a.Usage != nil && a.Usage.Allowance != "unknown" {
				t.Errorf("a Claude Code allowance must be unknown: %+v", a.Usage)
			}
			continue
		}
		if a.Usage == nil {
			t.Fatalf("codex account: %+v", a)
		}
		u := a.Usage
		if u.Source != "live" || u.Allowance != "blocked" || u.AllowanceReason != "usage_limit_reached" ||
			len(u.BlockedFeatures) != 1 || u.BlockedFeatures[0] != "code review" {
			t.Errorf("usage = %+v", u)
		}
		if a.Plan != "prolite" {
			t.Errorf("plan = %q, want the one headroom's own response named", a.Plan)
		}
		if len(u.Limits) != 3 {
			t.Fatalf("limits = %+v", u.Limits)
		}
		if extra := u.Limits[2]; extra.Feature != "astra_model" || extra.Group != "additional" || extra.WindowSeconds != 86400 {
			t.Errorf("additional limit = %+v", extra)
		}
		un, wk := u.Limits[0], u.Limits[1]
		if !un.Unstarted || un.ResetState != "none" || un.ResetsAt != nil || un.WindowSeconds != 18000 || un.Kind != "primary" || un.Group != "rate_limit" {
			t.Errorf("unstarted limit = %+v", un)
		}
		if wk.Unstarted || wk.ResetState != "ok" || wk.ResetsAt == nil || wk.WindowSeconds != 604800 || wk.Kind != "secondary" {
			t.Errorf("weekly limit = %+v", wk)
		}
		for _, l := range u.Limits {
			if l.Severity != "normal" {
				t.Errorf("a Codex limit's severity is always normal: %q", l.Severity)
			}
			for _, st := range []string{l.PercentState, l.ResetState, l.IdentityState} {
				if st != "ok" && st != "none" && st != "bad" {
					t.Errorf("a *_state field took a fourth value: %q", st)
				}
			}
		}
	}
	if vendors["claude"] != 3 || vendors["codex"] != 2 {
		t.Errorf("accounts by vendor = %v, want one flat list of both", vendors)
	}

	// limits emits the same document from disk alone, and replays the body.
	var buf bytes.Buffer
	if code := limitsTo(&buf, both, ""); code != 0 {
		t.Fatalf("limits exit %d", code)
	}
	ld := decode5(t, buf.Bytes())
	if len(ld.Accounts) != 5 {
		t.Errorf("limits: %d accounts", len(ld.Accounts))
	}
	for _, a := range ld.Accounts {
		if a.Vendor == "codex" && a.Name == "u1@x.com" && (a.Usage == nil || a.Usage.Source != "headroom_cache" || a.Usage.Allowance != "blocked") {
			t.Errorf("limits did not replay the stored Codex body: %+v", a.Usage)
		}
		// The disk-only surface knows the plan as well as the fetching one:
		// the stored response's when there is one, the login's otherwise.
		if a.Vendor == "codex" && a.Name == "u1@x.com" && a.Plan != "prolite" {
			t.Errorf("limits plan = %q, want the stored response's", a.Plan)
		}
		if a.Usage != nil && a.Usage.Source == "claude_cache" && a.Vendor == "codex" {
			t.Error("a Codex account reported claude_cache")
		}
	}

	// --vendor restricts accounts and problems to the one vendor.
	buf.Reset()
	if code := limitsTo(&buf, []config.Scope{f.scope}, ""); code != 0 {
		t.Fatal("limits --vendor codex failed")
	}
	only := decode5(t, buf.Bytes())
	if len(only.Accounts) != 2 {
		t.Errorf("filtered: %d accounts", len(only.Accounts))
	}
	for _, a := range only.Accounts {
		if a.Vendor != "codex" {
			t.Errorf("filtered document holds a %s account", a.Vendor)
		}
	}

	// --account filters by name inside the selected vendors: an email that is
	// an account of both answers twice, each under its own vendor.
	buf.Reset()
	if code := limitsTo(&buf, both, "u1@x.com"); code != 0 {
		t.Fatal("limits --account failed")
	}
	named := decode5(t, buf.Bytes())
	if len(named.Accounts) != 2 || named.Accounts[0].Vendor == named.Accounts[1].Vendor {
		t.Errorf("--account across vendors: %+v", named.Accounts)
	}
	// A name only one vendor has still answers.
	buf.Reset()
	if code := limitsTo(&buf, both, "claude-only@x.com"); code != 0 {
		t.Fatal("limits --account claude-only@x.com failed")
	}
	if p := decode5(t, buf.Bytes()); len(p.Accounts) != 1 || p.Accounts[0].Vendor != "claude" {
		t.Errorf("--account claude-only@x.com: %+v", p.Accounts)
	}
	buf.Reset()
	if code := limitsTo(&buf, both, "nobody@x.com"); code != 1 || buf.Len() != 0 {
		t.Errorf("an unknown name: exit %d, wrote %q", code, buf.String())
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

// An unreadable store must be distinguishable from "nothing has ever been
// observed": both serialize usage as null, and only the problems section says
// which one the consumer is looking at — in the `limits` document and the
// `--json` board's alike, since each builds its board from its own read of
// the store. A problem is attributed to the vendor whose file it is, --vendor
// filters problems with everything else, and a healthy store carries none:
// the section is a signal, not a fixture. Dropping the store's problems from
// either surface's board must fail here.
func TestSchema5ProblemsCarryTheirVendor(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1")
	f.extra(t, login(1))
	t.Setenv("PATH", t.TempDir()) // the board's probes and credential reads answer "unavailable"
	limits := func(scopes ...config.Scope) []byte {
		t.Helper()
		var buf bytes.Buffer
		if code := limitsTo(&buf, scopes, ""); code != 0 {
			t.Fatal("limits failed")
		}
		return buf.Bytes()
	}
	board := func(scopes ...config.Scope) []byte {
		t.Helper()
		data, err := jsonDocument(fetchBoards(scopes, BoardDeps{Round: LiveRound(true), Place: quietPlace()}), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// The file's own sections. A request the board made against the broken
	// ledger reports its bookkeeping too, as request[<name>]; that is the
	// attempt's problem, not the file's.
	fileProblems := func(d doc5) (out []string) {
		for _, p := range d.Problems {
			if !strings.HasPrefix(p.Section, "request[") {
				out = append(out, p.Vendor+":"+p.Section)
			}
		}
		return out
	}
	both := []config.Scope{f.cfg.Claude, f.scope}
	claudeState := filepath.Join(f.cfg.Claude.AccountsRoot, "state.json")
	codexState := filepath.Join(f.scope.AccountsRoot, "state.json")
	if err := os.MkdirAll(f.cfg.Claude.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	for surface, run := range map[string]func(...config.Scope) []byte{"limits": limits, "--json": board} {
		doc := func(scopes ...config.Scope) doc5 { t.Helper(); return decode5(t, run(scopes...)) }
		for _, file := range []string{claudeState, codexState} {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		var clean map[string]json.RawMessage
		if err := json.Unmarshal(run(both...), &clean); err != nil {
			t.Fatal(err)
		}
		if p, present := clean["problems"]; present {
			t.Errorf("%s: a healthy store must omit problems: %s", surface, p)
		}

		// One section of Codex's file unreadable: one problem, under its vendor.
		writeJSON(t, codexState, `{"version":1,"accounts":"broken"}`)
		if got := fileProblems(doc(both...)); len(got) != 1 || got[0] != "codex:accounts" {
			t.Errorf("%s: problems = %v, want the one Codex problem under its vendor", surface, got)
		}
		if d := doc(f.cfg.Claude); len(d.Problems) != 0 {
			t.Errorf("%s: --vendor claude reported Codex's problem: %+v", surface, d.Problems)
		}

		// Claude Code's whole file unreadable: still said, under its vendor.
		writeJSON(t, claudeState, `not json at all`)
		got := fileProblems(doc(f.cfg.Claude))
		if len(got) == 0 {
			t.Errorf("%s: corrupt store invisible: a first run and an unreadable file serialize alike", surface)
		}
		for _, p := range got {
			if !strings.HasPrefix(p, "claude:") {
				t.Errorf("%s: a problem in Claude Code's file filed as %q", surface, p)
			}
		}
	}
}

// limitsTo prints the limits document over scopes, narrowed to account when
// it is set.
func limitsTo(buf *bytes.Buffer, scopes []config.Scope, account string) int {
	got := call(func(std Streams) error { return Limits(std, scopes, account) })
	buf.WriteString(got.stdout)
	return got.code
}
