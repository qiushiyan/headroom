package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
)

type doc5 struct {
	Schema   int               `json:"schema"`
	Current  map[string]string `json:"current"`
	Accounts []struct {
		Vendor  string `json:"vendor"`
		Name    string `json:"name"`
		Current bool   `json:"current"`
		Usage   *struct {
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
		Vendor string `json:"vendor"`
	} `json:"problems"`
}

func decode5(t *testing.T, data []byte) doc5 {
	t.Helper()
	var d doc5
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, data)
	}
	// `current` must be an object, never the schema-4 string.
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	if len(raw["current"]) == 0 || raw["current"][0] != '{' {
		t.Fatalf("current is not an object: %s", raw["current"])
	}
	return d
}

// Obligation 15: the schema 5 contract over a two-vendor fixture, from the
// fetching surface and from `limits`, unfiltered and filtered.
func TestSchema5TwoVendors(t *testing.T) {
	body := `{"plan_type":"pro","account_id":"acct-1","user_id":"user-1",
	  "rate_limit":{"allowed":false,"limit_reached":true,
	    "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000,"reset_at":1790018000},
	    "secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":5000,"reset_at":1790239759}},
	  "code_review_rate_limit":{"allowed":false,"limit_reached":false},
	  "spend_control":{"reached":false},"rate_limit_reached_type":"usage_limit_reached"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	f := newCodexFixture(t, srv.URL)
	f.extra(t, login(1))
	// The same email as an account of both vendors.
	if err := os.MkdirAll(filepath.Join(f.cfg.Claude.AccountsRoot, "u1@x.com"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.scope.CurrentFile(), []byte("u1@x.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no claude, no security: probes answer "unavailable"
	both := []config.Scope{f.cfg.Claude, f.scope}

	data, err := jsonDocument(fetchBoards(both), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := decode5(t, data)
	if d.Schema != 5 {
		t.Errorf("schema = %d", d.Schema)
	}
	if d.Current["claude"] != "primary" || d.Current["codex"] != "u1@x.com" || len(d.Current) != 2 {
		t.Errorf("current = %v", d.Current)
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
		if !a.Current || a.Usage == nil {
			t.Fatalf("codex account: %+v", a)
		}
		u := a.Usage
		if u.Source != "live" || u.Allowance != "blocked" || u.AllowanceReason != "usage_limit_reached" ||
			len(u.BlockedFeatures) != 1 || u.BlockedFeatures[0] != "code review" {
			t.Errorf("usage = %+v", u)
		}
		if len(u.Limits) != 2 {
			t.Fatalf("limits = %+v", u.Limits)
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
	if vendors["claude"] != 2 || vendors["codex"] != 2 {
		t.Errorf("accounts by vendor = %v, want one flat list of both", vendors)
	}

	// limits emits the same document from disk alone, and replays the body.
	var buf bytes.Buffer
	if code := runLimitsTo(&buf, both, nil); code != 0 {
		t.Fatalf("limits exit %d", code)
	}
	ld := decode5(t, buf.Bytes())
	if len(ld.Accounts) != 4 || ld.Current["codex"] != "u1@x.com" {
		t.Errorf("limits: %d accounts, current %v", len(ld.Accounts), ld.Current)
	}
	for _, a := range ld.Accounts {
		if a.Vendor == "codex" && a.Name == "u1@x.com" && (a.Usage == nil || a.Usage.Source != "headroom_cache" || a.Usage.Allowance != "blocked") {
			t.Errorf("limits did not replay the stored Codex body: %+v", a.Usage)
		}
		if a.Usage != nil && a.Usage.Source == "claude_cache" && a.Vendor == "codex" {
			t.Error("a Codex account reported claude_cache")
		}
	}

	// --vendor restricts accounts, current and problems to the one vendor.
	buf.Reset()
	if code := runLimitsTo(&buf, []config.Scope{f.scope}, nil); code != 0 {
		t.Fatal("limits --vendor codex failed")
	}
	only := decode5(t, buf.Bytes())
	if len(only.Current) != 1 || only.Current["codex"] != "u1@x.com" || len(only.Accounts) != 2 {
		t.Errorf("filtered: current %v accounts %d", only.Current, len(only.Accounts))
	}
	for _, a := range only.Accounts {
		if a.Vendor != "codex" {
			t.Errorf("filtered document holds a %s account", a.Vendor)
		}
	}

	// --account filters by name inside the selected vendors: an email that is
	// an account of both answers twice, each under its own vendor.
	buf.Reset()
	if code := runLimitsTo(&buf, both, []string{"--account", "u1@x.com"}); code != 0 {
		t.Fatal("limits --account failed")
	}
	named := decode5(t, buf.Bytes())
	if len(named.Accounts) != 2 || named.Accounts[0].Vendor == named.Accounts[1].Vendor || len(named.Current) != 2 {
		t.Errorf("--account across vendors: %+v current %v", named.Accounts, named.Current)
	}
	// A name only one vendor has still answers, and the other vendor stays in
	// the envelope with no accounts.
	buf.Reset()
	if code := runLimitsTo(&buf, both, []string{"--account", "primary"}); code != 0 {
		t.Fatal("limits --account primary failed")
	}
	if p := decode5(t, buf.Bytes()); len(p.Accounts) != 2 || len(p.Current) != 2 {
		t.Errorf("--account primary: %+v", p.Accounts)
	}
	buf.Reset()
	if code := runLimitsTo(&buf, both, []string{"--account", "nobody@x.com"}); code != 1 || buf.Len() != 0 {
		t.Errorf("an unknown name: exit %d, wrote %q", code, buf.String())
	}
}

// Obligation 13: a machine without Codex gets schema 5 with Claude Code only.
func TestSchema5WithoutCodex(t *testing.T) {
	cfg := config.ForHome(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if got := cfg.Present(); len(got) != 1 {
		t.Fatalf("present = %v", got)
	}
	data, err := jsonDocument(fetchBoards(cfg.Present()), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := decode5(t, data)
	if d.Schema != 5 || len(d.Current) != 1 || len(d.Accounts) != 1 || d.Accounts[0].Vendor != "claude" {
		t.Errorf("doc = %+v", d)
	}
	if _, ok := d.Current["codex"]; ok {
		t.Error("an absent vendor appeared in current")
	}
}

func TestTakeVendor(t *testing.T) {
	for _, c := range []struct {
		args   []string
		vendor config.Vendor
		set    bool
		rest   string
		bad    bool
	}{
		{[]string{"--account", "a"}, config.Claude, false, "--account a", false},
		{[]string{"--vendor", "codex", "--account", "a"}, config.Codex, true, "--account a", false},
		{[]string{"--account", "a", "--vendor=codex"}, config.Codex, true, "--account a", false},
		{[]string{"--vendor", "claude"}, config.Claude, true, "", false},
		// Whatever follows -- belongs to the launched child.
		{[]string{"--", "--vendor", "codex"}, config.Claude, false, "-- --vendor codex", false},
		{[]string{"--vendor", "codex", "--", "resume", "--all"}, config.Codex, true, "-- resume --all", false},
		{[]string{"--vendor"}, "", false, "", true},
		{[]string{"--vendor", "gemini"}, "", false, "", true},
	} {
		v, set, rest, err := takeVendor(c.args)
		if (err != nil) != c.bad {
			t.Errorf("%v: err = %v", c.args, err)
			continue
		}
		if c.bad {
			continue
		}
		if v != c.vendor || set != c.set || join(rest) != c.rest {
			t.Errorf("%v: (%q, %v, %q)", c.args, v, set, join(rest))
		}
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
