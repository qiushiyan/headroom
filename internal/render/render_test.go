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

// Health must reach the user whether or not rows exist. An account that needs
// /login but happens to have a recent cache would otherwise render as ordinary
// coloured bars with no warning — and the picker would offer it as a live
// choice, which is precisely the class of mistake this model exists to stop.
func TestNonOKHealthSurfacesEvenWithRows(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	base := AccountView{
		Label: "a@x.com", Launcher: "x-a",
		Obs: &Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 12, Severity: "normal"}},
			ObservedAt: now - 5, Source: SourceCache,
		},
		Attempt: Attempt{State: AttemptNone},
	}
	for _, c := range []struct {
		health Health
		want   string
	}{
		{HealthNoLogin, "/login"},
		{HealthReloginRequired, "/login"},
		{HealthBadBlob, "headroom check"},
		{HealthUnknown, "headroom check"},
	} {
		v := base
		v.Health = c.health
		joined := strings.Join(p.AccountBlock(v, now, 16), "\n")
		if !strings.Contains(joined, c.want) {
			t.Errorf("health %v hidden behind cached rows; want %q in:\n%s", c.health, c.want, joined)
		}
	}
}

// "Safe to pick" is health AND freshness. Age alone was letting an unusable
// account through on a recent cache.
func TestActionableRequiresHealthAndFreshness(t *testing.T) {
	now := time.Now().Unix()
	fresh := &Observation{Rows: []usage.Row{{Label: "5h session"}}, ObservedAt: now - 5}
	if (AccountView{Health: HealthNoLogin, Obs: fresh}).Actionable(now) {
		t.Error("a logged-out account with a fresh cache was reported as actionable")
	}
	if !(AccountView{Health: HealthOK, Obs: fresh}).Actionable(now) {
		t.Error("a healthy account with fresh figures should be actionable")
	}
	old := &Observation{Rows: []usage.Row{{Label: "5h session"}}, ObservedAt: now - 100000}
	if (AccountView{Health: HealthOK, Obs: old}).Actionable(now) {
		t.Error("stale figures are not grounds for a choice")
	}
}

// The caption is a composition of independent clauses, never one verdict.
// The trap: a single state per account has no cell for "current figures whose
// newest refresh was refused" or "current figures from Claude Code's cache",
// so both facts vanish behind bars that look freshly fetched.
func TestProvenanceKeepsTheAxesApart(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	obs := func(age time.Duration, src Source) *Observation {
		return &Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 8}},
			ObservedAt: now - int64(age.Seconds()),
			Source:     src,
		}
	}
	cases := []struct {
		name    string
		view    AccountView
		want    []string // substrings the caption must carry
		notWant []string
	}{
		{
			name: "current and ours says nothing",
			view: AccountView{Obs: obs(20*time.Second, SourceLive), Attempt: Attempt{State: AttemptOK}},
		},
		{
			name: "current, replayed from our own store, still says nothing",
			view: AccountView{Obs: obs(20*time.Second, SourceStore), Attempt: Attempt{State: AttemptNone}},
		},
		{
			name: "a deferred refresh over current figures is a non-event",
			view: AccountView{Obs: obs(20*time.Second, SourceStore), Attempt: Attempt{State: AttemptDeferred}},
		},
		{
			name:    "current figures whose refresh was refused still report it",
			view:    AccountView{Obs: obs(20*time.Second, SourceLive), Attempt: Attempt{State: AttemptRefused}},
			want:    []string{"observed 20s ago", "rate limited"},
			notWant: []string{"stale"},
		},
		{
			name:    "current figures from the vendor cache still name their source",
			view:    AccountView{Obs: obs(20*time.Second, SourceCache), Attempt: Attempt{State: AttemptOK}},
			want:    []string{"via Claude Code's cache"},
			notWant: []string{"stale"},
		},
		{
			name: "old figures are nagged about, whoever fetched them",
			view: AccountView{Obs: obs(22*time.Hour, SourceStore), Attempt: Attempt{State: AttemptDeferred}},
			want: []string{"stale", "observed 22h ago", "live check deferred"},
		},
		{
			name:    "a refresh in flight shows over the figures it will replace",
			view:    AccountView{Obs: obs(20*time.Second, SourceStore), Attempt: Attempt{State: AttemptPending}},
			want:    []string{"fetching"},
			notWant: []string{"stale"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.ProvenanceLine(c.view, now)
			if len(c.want) == 0 && got != "" {
				t.Fatalf("expected no caption, got %q", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("caption %q is missing %q", got, w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("caption %q should not carry %q", got, w)
				}
			}
		})
	}
}

// A row whose own reset instant has passed describes a window that has ended.
// The percent is a fact about the past, and a low one reads as headroom that
// may not exist — measured on the live machine as an 8% bar for a window that
// had rolled over 33 hours earlier.
func TestRolledOverRowIsNotAnAnswer(t *testing.T) {
	now := time.Now().Unix()
	p := NewPalette(false)
	rolled := usage.Row{Label: "5h session", Percent: 8, ResetAt: now - 33*3600,
		PercentState: usage.StateOK, ResetState: usage.StateOK}

	line := p.LimitRow(rolled, now, 12, false)
	if strings.Contains(line, "8%") {
		t.Errorf("a rolled-over window still shows its old percent: %q", line)
	}
	if !strings.Contains(line, "window rolled over") {
		t.Errorf("a rolled-over window must say so: %q", line)
	}
	if strings.Contains(line, "drift") {
		t.Errorf("rolling over is not drift — the field parsed: %q", line)
	}

	// And it is not grounds for a choice, however recently it was observed.
	v := AccountView{
		Health: HealthOK,
		Obs:    &Observation{Rows: []usage.Row{rolled}, ObservedAt: now - 5, Source: SourceLive},
	}
	if !v.Fresh(now) {
		t.Fatal("the observation itself is current; only the window ended")
	}
	if v.Actionable(now) {
		t.Error("an account whose window rolled over must not be offered as a live choice")
	}
}
