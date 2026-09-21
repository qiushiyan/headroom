package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/usage"
)

func codexFacts(t *testing.T, label, body string, now int64) accountstate.Facts {
	t.Helper()
	r, err := usage.Parse(config.Codex, []byte(body))
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return accountstate.Facts{
		Vendor: config.Codex, Label: label, Health: accountstate.HealthOK,
		Launcher:     "cx-" + label,
		LoginCommand: "headroom launch --vendor codex --account " + label + " -- login",
		Obs:          accountstate.FromReading(r, now-5, accountstate.SourceLive),
	}
}

const renderNow = int64(1_790_000_000)

// Codex columns run rate_limit first, then code review, then the additional
// limits by feature; inside a group the longer window comes first. Labels
// derive from the decoded fields, including an additional limit with an empty
// limit_name.
func TestCodexCompactColumnOrderAndLabels(t *testing.T) {
	body := `{"rate_limit":{"allowed":true,"limit_reached":false,
	    "primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":100,"reset_at":1790003600},
	    "secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":100,"reset_at":1790400000}},
	  "code_review_rate_limit":{"allowed":true,"limit_reached":false,
	    "primary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":100,"reset_at":1790400000}},
	  "additional_rate_limits":[
	    {"limit_name":"Zed","metered_feature":"zed","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":86400,"reset_after_seconds":100,"reset_at":1790080000}}},
	    {"limit_name":"","metered_feature":"astra_model","rate_limit":{"primary_window":{"used_percent":2,"limit_window_seconds":86400,"reset_after_seconds":100,"reset_at":1790080000}}}]}`
	p := NewPalette(false)
	b := p.Board([]accountstate.Facts{codexFacts(t, "a@x.com", body, renderNow)}, renderNow, LayoutCompact, 0)
	header := b.Header[0]
	want := []string{"weekly", "5h", "weekly code review", "1d astra_model", "1d Zed"}
	at := -1
	for _, w := range want {
		re := regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(w) + `(\s|$)`)
		loc := re.FindStringIndex(header[max(at, 0):])
		if loc == nil {
			t.Fatalf("heading %q missing or out of order in %q (want order %v)", w, header, want)
		}
		at = max(at, 0) + loc[1] - 1
	}
}

// Two accounts whose primary slots run for different lengths get different
// columns, and each figure sits under its own heading — never the other's.
func TestCodexDifferentDurationsAreDifferentColumns(t *testing.T) {
	win := func(pct, secs string) string {
		return `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":` + pct + `,"limit_window_seconds":` + secs + `,"reset_after_seconds":100,"reset_at":1790003600}}}`
	}
	p := NewPalette(false)
	b := p.Board([]accountstate.Facts{
		codexFacts(t, "a@x.com", win("71", "604800"), renderNow),
		codexFacts(t, "b@x.com", win("22", "18000"), renderNow),
	}, renderNow, LayoutCompact, 0)
	header := b.Header[0]
	// Columns are counted in cells, not bytes: the dash is a multi-byte rune.
	cellIndex := func(s, sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			return -1
		}
		return len([]rune(s[:i]))
	}
	weekly, fiveHour := cellIndex(header, "weekly"), cellIndex(header, "5h")
	if weekly < 0 || fiveHour < 0 || weekly > fiveHour {
		t.Fatalf("header = %q: want weekly before 5h", header)
	}
	// A cell's percent is right-aligned in a slot that starts at its heading.
	under := func(line string, col int) string {
		r := []rune(line)
		return strings.TrimSpace(string(r[col:min(col+pctWidth, len(r))]))
	}
	a, bRow := b.Groups[0][0], b.Groups[1][0]
	if under(a, weekly) != "71%" || under(a, fiveHour) != "—" {
		t.Errorf("a@x.com: weekly %q, 5h %q\n%s\n%s", under(a, weekly), under(a, fiveHour), header, a)
	}
	if under(bRow, weekly) != "—" || under(bRow, fiveHour) != "22%" {
		t.Errorf("b@x.com: weekly %q, 5h %q\n%s\n%s", under(bRow, weekly), under(bRow, fiveHour), header, bRow)
	}
}

// An unstarted window shows 0% and "not started" where the countdown would
// be, in both layouts, and never a countdown of one full window.
func TestCodexUnstartedWindow(t *testing.T) {
	body := `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800,"reset_at":1790604800}}}`
	v := codexFacts(t, "fresh@x.com", body, renderNow)
	p := NewPalette(false)
	for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
		out := boardText(p.Board([]accountstate.Facts{v}, renderNow, layout, 0))
		if !strings.Contains(out, "0%") || !strings.Contains(out, "not started") {
			t.Errorf("layout %d:\n%s", layout, out)
		}
		if strings.Contains(out, "7.0d") || strings.Contains(out, "resets") || strings.Contains(out, "rolled") {
			t.Errorf("layout %d shows a countdown for a window nobody started:\n%s", layout, out)
		}
	}
}

// Positive blocking evidence is said in a caption naming the reason; a block
// on one feature is named too and leaves the account usable.
func TestCodexAllowanceCaptions(t *testing.T) {
	blocked := codexFacts(t, "blocked@x.com", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":10,"reset_at":1790400000}},"spend_control":{"reached":true}}`, renderNow)
	feature := codexFacts(t, "feature@x.com", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":10,"reset_at":1790400000}},"code_review_rate_limit":{"allowed":false,"limit_reached":true}}`, renderNow)
	reason := codexFacts(t, "ws@x.com", `{"rate_limit":null,"rate_limit_reached_type":{"type":"workspace_owner_usage_limit_reached"}}`, renderNow)
	drift := codexFacts(t, "drift@x.com", `{"rate_limit":{"allowed":"yes","limit_reached":false}}`, renderNow)
	p := NewPalette(false)
	for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
		out := func(v accountstate.Facts) string {
			return boardText(p.Board([]accountstate.Facts{v}, renderNow, layout, 0))
		}
		if s := out(blocked); !strings.Contains(s, "blocked — spend limit reached") {
			t.Errorf("layout %d, blocked:\n%s", layout, s)
		}
		if s := out(reason); !strings.Contains(s, "blocked — workspace owner usage limit reached") {
			t.Errorf("layout %d, vendor reason:\n%s", layout, s)
		}
		s := out(feature)
		if !strings.Contains(s, "code review blocked") || strings.Contains(s, "blocked —") {
			t.Errorf("layout %d, feature-only block:\n%s", layout, s)
		}
		if s := out(drift); !strings.Contains(s, "drift") {
			t.Errorf("layout %d, a malformed allowance is not marked as drift:\n%s", layout, s)
		}
	}
	if blocked.Actionable(renderNow) || !feature.Actionable(renderNow) {
		t.Error("actionability does not follow the allowance")
	}
}

// Codex has no /login command. Every hint that tells a person to log in names
// the engine's own launch spelling — never the configured launcher format,
// never /login.
func TestCodexLoginHints(t *testing.T) {
	p := NewPalette(false)
	for _, h := range []accountstate.Health{accountstate.HealthNoLogin, accountstate.HealthReloginRequired} {
		v := accountstate.Facts{Vendor: config.Codex, Label: "a@x.com", Launcher: "cx-a@x.com", Health: h,
			LoginCommand: "headroom launch --vendor codex --account a@x.com -- login"}
		for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
			out := boardText(p.Board([]accountstate.Facts{v}, renderNow, layout, 0))
			if !strings.Contains(out, "run headroom launch --vendor codex --account a@x.com -- login") {
				t.Errorf("health %d layout %d:\n%s", h, layout, out)
			}
			if strings.Contains(out, "/login") || strings.Contains(out, "run cx-") {
				t.Errorf("health %d layout %d names /login or the wrapper:\n%s", h, layout, out)
			}
		}
	}
	// Claude Code's hint is unchanged.
	v := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthNoLogin}
	if out := boardText(p.Board([]accountstate.Facts{v}, renderNow, LayoutBlocks, 0)); !strings.Contains(out, "not logged in — run x-a and /login") {
		t.Errorf("claude hint changed:\n%s", out)
	}
}

// A 401 is an attempt outcome with its own words; another auth mode names
// the mode; an unidentified account names the right document.
func TestCodexAttemptAndModeCaptions(t *testing.T) {
	p := NewPalette(false)
	rejected := accountstate.Facts{Vendor: config.Codex, Label: "a@x.com", Launcher: "cx-a", Health: accountstate.HealthOK,
		Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}}
	if out := boardText(p.Board([]accountstate.Facts{rejected}, renderNow, LayoutBlocks, 0)); !strings.Contains(out, "access token rejected — any cx-a session refreshes it") || strings.Contains(out, "login") {
		t.Errorf("401:\n%s", out)
	}
	apikey := accountstate.Facts{Vendor: config.Codex, Label: "k@x.com", Health: accountstate.HealthUnknown, AuthMode: "apikey"}
	if out := boardText(p.Board([]accountstate.Facts{apikey}, renderNow, LayoutCompact, 0)); !strings.Contains(out, "auth mode apikey") {
		t.Errorf("auth mode:\n%s", out)
	}
	unknown := accountstate.Facts{Vendor: config.Codex, Label: "u@x.com", Health: accountstate.HealthOK,
		Attempt: accountstate.Attempt{State: accountstate.AttemptIdentityUnknown}}
	if out := boardText(p.Board([]accountstate.Facts{unknown}, renderNow, LayoutBlocks, 0)); !strings.Contains(out, "auth.json") || strings.Contains(out, ".claude.json") {
		t.Errorf("identity unknown:\n%s", out)
	}
}

func boardText(b Board) string {
	var sb strings.Builder
	for _, l := range b.Header {
		sb.WriteString(l + "\n")
	}
	for _, g := range b.Groups {
		for _, l := range g {
			sb.WriteString(l + "\n")
		}
	}
	return sb.String()
}
