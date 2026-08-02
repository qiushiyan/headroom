package render

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/usage"
)

func TestBar(t *testing.T) {
	cases := []struct {
		pct    int
		filled int
	}{
		{0, 0}, {3, 1}, {50, 10}, {56, 11}, {100, 20},
		{130, 20}, // clamped high
		{-5, 0},   // clamped low
	}
	for _, c := range cases {
		bar := Bar(c.pct)
		if got := strings.Count(bar, "█"); got != c.filled {
			t.Errorf("Bar(%d): %d filled, want %d", c.pct, got, c.filled)
		}
		if got := strings.Count(bar, "░"); got != BarWidth-c.filled {
			t.Errorf("Bar(%d): %d empty, want %d", c.pct, got, BarWidth-c.filled)
		}
	}
}

func TestResetPhrase(t *testing.T) {
	now := int64(1_754_000_000)
	if got := ResetPhrase(0, now); got != "resets ?" {
		t.Errorf("unknown: %q", got)
	}
	if got := ResetPhrase(now-10, now); got != "resetting…" {
		t.Errorf("past: %q", got)
	}
	sameDay := regexp.MustCompile(`^resets \d{2}:\d{2} \(in (.+)\)$`)
	withDay := regexp.MustCompile(`^resets [A-Z][a-z]{2} \d{2}:\d{2} \(in (.+)\)$`)
	cases := []struct {
		rem int64
		re  *regexp.Regexp
		in  string
	}{
		{90, sameDay, "1m"},
		{3*3600 + 120, sameDay, "3h 2m"},
		{2*86400 + 3*3600, withDay, "2d 3h"},
	}
	for _, c := range cases {
		got := ResetPhrase(now+c.rem, now)
		m := c.re.FindStringSubmatch(got)
		if m == nil {
			t.Errorf("ResetPhrase(+%d) = %q, want match %v", c.rem, got, c.re)
			continue
		}
		if m[1] != c.in {
			t.Errorf("ResetPhrase(+%d) = %q, want remaining %q", c.rem, got, c.in)
		}
	}
}

func TestLimitRowPlain(t *testing.T) {
	p := NewPalette(false)
	row := usage.Row{Label: "5h session", Percent: 56, Severity: "normal"}
	got := p.LimitRow(row, 0, 16, false)
	if !strings.HasPrefix(got, "  5h session       [") {
		t.Errorf("label padding off: %q", got)
	}
	if !strings.Contains(got, "]  56%  resets ?") {
		t.Errorf("row = %q", got)
	}
	// A wider column pads further.
	got = p.LimitRow(row, 0, 20, false)
	if !strings.HasPrefix(got, "  5h session           [") {
		t.Errorf("wide label padding off: %q", got)
	}
}

// A drift-tagged field must be visibly different from a genuine value — a
// bad percent rendering as a plain 0% would read as free headroom.
func TestLimitRowDrift(t *testing.T) {
	p := NewPalette(false)
	bad := p.LimitRow(usage.Row{Label: "5h session", Severity: "normal", PercentState: usage.StateBad}, 0, 16, false)
	real0 := p.LimitRow(usage.Row{Label: "5h session", Percent: 0, Severity: "normal"}, 0, 16, false)
	if bad == real0 {
		t.Fatalf("bad percent indistinguishable from real 0%%: %q", bad)
	}
	if strings.Contains(bad, "0%") || !strings.Contains(bad, "?%") {
		t.Errorf("bad percent should render ?%%, got %q", bad)
	}
	if strings.Contains(bad, "░") {
		t.Errorf("bad percent should not render an empty (all-headroom) bar: %q", bad)
	}
	if !strings.Contains(bad, "drift") || strings.Contains(real0, "drift") {
		t.Errorf("drift marker wrong: bad=%q real0=%q", bad, real0)
	}

	// A bad timestamp alone also carries the marker.
	badReset := p.LimitRow(usage.Row{Label: "5h session", Percent: 5, Severity: "normal",
		ResetState: usage.StateBad}, 0, 16, false)
	if !strings.Contains(badReset, "drift") {
		t.Errorf("bad reset should carry the drift marker: %q", badReset)
	}
}

func TestLabelWidth(t *testing.T) {
	short := AccountView{Obs: &Observation{Rows: []usage.Row{{Label: "5h session"}}}}
	long := AccountView{Obs: &Observation{Rows: []usage.Row{{Label: "Claude Opus 4.5 (7d)"}}}}
	if got := LabelWidth([]AccountView{short}); got != 16 {
		t.Errorf("short labels should keep the classic column: %d", got)
	}
	if got := LabelWidth([]AccountView{short, long}); got != len("Claude Opus 4.5 (7d)") {
		t.Errorf("width should follow the longest label: %d", got)
	}
}

func TestClip(t *testing.T) {
	p := NewPalette(true)
	cases := []struct {
		name, in string
		width    int
		want     string
	}{
		{"short unchanged", "abc", 5, "abc"},
		{"exact unchanged", "abcde", 5, "abcde"},
		{"plain truncated", "abcdef", 4, "abcd"},
		{"zero width passes through", "abc", 0, "abc"},
		{"escapes not counted", p.Red + "abc" + p.Rst, 3, p.Red + "abc" + p.Rst},
		{"cut keeps later escapes", p.Red + "abcdef" + p.Rst + "gh", 4, p.Red + "abcd" + p.Rst},
		{"multibyte runes", "ééééé", 3, "ééé"},
		{"bar glyphs", "[████░░]", 5, "[████"},
	}
	for _, c := range cases {
		if got := Clip(c.in, c.width); got != c.want {
			t.Errorf("%s: Clip(%q, %d) = %q, want %q", c.name, c.in, c.width, got, c.want)
		}
	}
}

func TestHeaderLine(t *testing.T) {
	p := NewPalette(false)
	v := AccountView{Label: "a@b.c", Plan: "max 20x", Launcher: "x-a", Current: true}
	if got := p.HeaderLine(v); got != "a@b.c (max 20x · x-a)  ← x" {
		t.Errorf("header = %q", got)
	}
	v = AccountView{Label: "a@b.c", Launcher: "x-a"}
	if got := p.HeaderLine(v); got != "a@b.c  x-a" {
		t.Errorf("planless header = %q", got)
	}
	v = AccountView{Label: "real@b.c", DirMismatch: "dir@b.c", Launcher: "x-a"}
	if got := p.HeaderLine(v); !strings.Contains(got, "real@b.c (dir says dir@b.c!)") {
		t.Errorf("mismatch header = %q", got)
	}
}

// The reported bug, at the rendering layer: a refused refresh must annotate
// what is known, never replace it. Before the three-axis model the account
// block for a 429 was a single "HTTP 429" line and the bars vanished.
func TestAccountBlockKeepsRowsThroughAFailedRefresh(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	v := AccountView{
		Label: "a@x.com", Launcher: "x-a", Health: HealthOK,
		Obs: &Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 42, Severity: "normal"}},
			ObservedAt: now - 30, Source: SourceLive,
		},
		Attempt: Attempt{State: AttemptRefused, HTTPCode: 429, NextEligibleAt: now + 180},
	}
	lines := p.AccountBlock(v, now, 16)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "42%") {
		t.Errorf("known figures erased by a refused refresh:\n%s", joined)
	}
	if !strings.Contains(joined, "rate limited") {
		t.Errorf("refusal not explained:\n%s", joined)
	}
	if !strings.Contains(joined, "next attempt in 3m") {
		t.Errorf("no indication of when it retries:\n%s", joined)
	}
}

// A stale access token is routine housekeeping. It must never be phrased as
// expiry or send the user to /login — that was the false alarm users learned
// to distrust.
func TestStaleTokenReadsAsHousekeeping(t *testing.T) {
	p := NewPalette(false)
	v := AccountView{Label: "a@x.com", Launcher: "x-a", Health: HealthOK,
		Attempt: Attempt{State: AttemptTokenStale}}
	line := p.StatusLine(v, time.Now().Unix())
	if strings.Contains(line, "expired") || strings.Contains(line, "/login") {
		t.Errorf("stale access token phrased as an account problem: %q", line)
	}
	if !strings.Contains(line, "x-a") || !strings.Contains(line, "refreshes it") {
		t.Errorf("remedy not stated: %q", line)
	}

	// The genuine case still says what it must.
	dead := p.StatusLine(AccountView{Launcher: "x-a", Health: HealthReloginRequired}, 0)
	if !strings.Contains(dead, "/login") {
		t.Errorf("a dead refresh token must tell the user to log in: %q", dead)
	}
}

// Old figures render, but must be visibly marked so they cannot be read as
// current headroom at a glance.
func TestStaleObservationIsMarked(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	v := AccountView{Label: "a@x.com", Launcher: "x-a", Health: HealthOK,
		Obs: &Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 58, Severity: "normal"}},
			ObservedAt: now - int64((22 * time.Hour).Seconds()), Source: SourceCache,
		},
		Attempt: Attempt{State: AttemptDeferred, NextEligibleAt: now + 40},
	}
	joined := strings.Join(p.AccountBlock(v, now, 16), "\n")
	if !strings.Contains(joined, "58%") {
		t.Errorf("stale figures should still be shown as context:\n%s", joined)
	}
	if !strings.Contains(joined, "stale") || !strings.Contains(joined, "22h ago") {
		t.Errorf("staleness not surfaced:\n%s", joined)
	}
	if !strings.Contains(joined, "Claude Code's cache") {
		t.Errorf("source not surfaced:\n%s", joined)
	}
	if v.Fresh(now) {
		t.Error("22h-old observation reported as fresh")
	}
}

// The ordinary case stays quiet: fresh live numbers need no caption.
func TestFreshLiveObservationHasNoProvenanceNoise(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	v := AccountView{Label: "a@x.com", Launcher: "x-a", Health: HealthOK,
		Obs: &Observation{Rows: []usage.Row{{Label: "5h session", Percent: 3, Severity: "normal"}},
			ObservedAt: now - 2, Source: SourceLive},
		Attempt: Attempt{State: AttemptOK},
	}
	if got := p.ProvenanceLine(v, now); got != "" {
		t.Errorf("fresh live rows should need no caption, got %q", got)
	}
	if len(p.AccountBlock(v, now, 16)) != 2 {
		t.Errorf("expected header + one row only: %#v", p.AccountBlock(v, now, 16))
	}
}
