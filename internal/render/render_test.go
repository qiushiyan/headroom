package render

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/tag"
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
		glyphs := bar(c.pct)
		if got := strings.Count(glyphs, "█"); got != c.filled {
			t.Errorf("bar(%d): %d filled, want %d", c.pct, got, c.filled)
		}
		if got := strings.Count(glyphs, "░"); got != barWidth-c.filled {
			t.Errorf("bar(%d): %d empty, want %d", c.pct, got, barWidth-c.filled)
		}
	}
}

// The block's reset phrase: a reset still ahead is a clock — the time left in
// Remaining's spelling, with the weekday once it is a day or more away — and
// a reset nobody stated is never one. (The rolled-over and unstarted phrases
// are owned by TestRolledOverRowIsNotAnAnswer and TestCodexUnstartedWindow.)
func TestResetPhrase(t *testing.T) {
	p := NewPalette(false)
	now := int64(1_754_000_000)
	row := func(resetAt int64) string {
		return p.limitRow(usage.Row{Label: "5h session", Percent: 5, Severity: "normal", ResetAt: resetAt}, now, 16)
	}
	if got := row(0); !strings.Contains(got, "resets ?") || strings.Contains(got, "(in ") {
		t.Errorf("absent reset drew a clock: %q", got)
	}
	for _, rem := range []int64{90, 3*3600 + 6*60, 23 * 3600, 86400, 2*86400 + 3*3600} {
		got := row(now + rem)
		if !strings.Contains(got, "(in "+Remaining(rem)+")") {
			t.Errorf("+%ds: %q lacks the time left %q", rem, got, Remaining(rem))
		}
		day := time.Unix(now+rem, 0).Format("Mon")
		if want := rem >= 86400; strings.Contains(got, day) != want {
			t.Errorf("+%ds: weekday %s shown should be %v: %q", rem, day, want, got)
		}
	}
}

// A drift-tagged field must be visibly different from a genuine value — a
// bad percent rendering as a plain 0% would read as free headroom.
func TestLimitRowDrift(t *testing.T) {
	p := NewPalette(false)
	bad := p.limitRow(usage.Row{Label: "5h session", Severity: "normal", PercentState: tag.Bad}, 0, 16)
	real0 := p.limitRow(usage.Row{Label: "5h session", Percent: 0, Severity: "normal"}, 0, 16)
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
	badReset := p.limitRow(usage.Row{Label: "5h session", Percent: 5, Severity: "normal",
		ResetState: tag.Bad}, 0, 16)
	if !strings.Contains(badReset, "drift") {
		t.Errorf("bad reset should carry the drift marker: %q", badReset)
	}

	// A bad percent whose window has since ended says both, as the compact
	// cell does: the percent is drift, and the window rolled over.
	now := int64(1_800_000_000)
	ended := p.limitRow(usage.Row{Label: "5h session", Severity: "normal", PercentState: tag.Bad, ResetAt: now - 1800}, now, 16)
	if !strings.Contains(ended, "?%") || !strings.Contains(ended, "rolled over") || !strings.Contains(ended, "drift") {
		t.Errorf("bad percent over an ended window: %q", ended)
	}
}

// Every account's bars open in one column. The label column is as wide as
// the longest label on the whole board and never narrower than the classic
// one, so one account's model-scoped row cannot push its bars out of line
// with another account's — and it widens by exactly what that label needs.
func TestLabelWidth(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	long := scoped("Claude Opus 4.5", 70, now+90000)
	views := []accountstate.Facts{
		{Label: "a@x.com", Obs: fresh(now, session(12, now+3600), weeklyAll(40, now+90000)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "b@x.com", Obs: fresh(now, long), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	// barColumns is the cell at which each limit line's bar opens.
	barColumns := func(views []accountstate.Facts) []int {
		var cols []int
		for _, g := range p.Board(views, now, LayoutBlocks, 0).Groups {
			for _, line := range g {
				if before, _, ok := strings.Cut(line, "["); ok {
					cols = append(cols, Cells(before))
				}
			}
		}
		return cols
	}
	classic := barColumns(views[:1])
	if len(classic) != 2 || classic[0] != classic[1] {
		t.Fatalf("short labels: bars open at %v, want one column", classic)
	}
	got := barColumns(views)
	if len(got) != 3 {
		t.Fatalf("bars open at %v, want three bars", got)
	}
	for _, c := range got {
		if c != got[0] {
			t.Fatalf("bars open at %v: not one column across accounts", got)
		}
	}
	if widened := got[0] - classic[0]; widened != Cells(long.Label)-minLabelWidth {
		t.Errorf("the column widened by %d for %q, want %d", widened, long.Label, Cells(long.Label)-minLabelWidth)
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
		// A wide rune that would straddle the boundary is dropped whole:
		// 会(2)话(2) clipped to 3 cells is one rune, 2 cells — never a wrap.
		{"wide rune at the boundary is dropped whole", "会话选择", 3, "会"},
		{"mixed narrow and wide", "a会话", 4, "a会"},
	}
	for _, c := range cases {
		if got := Clip(c.in, c.width); got != c.want {
			t.Errorf("%s: Clip(%q, %d) = %q, want %q", c.name, c.in, c.width, got, c.want)
		}
	}
}

// The header names the account, its plan and its launcher, and carries the
// routing mark: "← current" on the account new sessions go to and, under
// automatic placement, "← next" on the one a bare launch would take — never
// both. The compact row carries the same two marks as ● and →. These marks
// are what the pty harness reads (accounts_auto.exp), so their spelling is
// pinned here and nowhere else.
func TestHeaderLine(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	for _, c := range []struct {
		name                  string
		plan                  string
		current, next         bool // the facts
		wantCurrent, wantNext bool // the marks
	}{
		{"current", "max 20x", true, false, true, false},
		{"next, under auto", "max 20x", false, true, false, true},
		{"a current account is never also marked next", "max 20x", true, true, true, false},
		{"neither, and no plan to name", "", false, false, false, false},
	} {
		v := accountstate.Facts{Label: "a@b.c", Plan: c.plan, Launcher: "x-a", Current: c.current, Next: c.next}
		header := p.Board([]accountstate.Facts{v}, now, LayoutBlocks, 0).Groups[0][0]
		for _, tok := range []string{"a@b.c", c.plan, "x-a"} {
			if !strings.Contains(header, tok) {
				t.Errorf("%s: header %q lacks %q", c.name, header, tok)
			}
		}
		if strings.Contains(header, "← current") != c.wantCurrent || strings.Contains(header, "← next") != c.wantNext {
			t.Errorf("%s: header = %q", c.name, header)
		}
		row := p.Board([]accountstate.Facts{v}, now, LayoutCompact, 0).Groups[0][0]
		if strings.Contains(row, "●") != c.wantCurrent || strings.Contains(row, "→") != c.wantNext {
			t.Errorf("%s: compact row = %q", c.name, row)
		}
	}
}

// The reported bug, at the rendering layer: a refused refresh must annotate
// what is known, never replace it. Before the three-axis model the account
// block for a 429 was a single "HTTP 429" line and the bars vanished.
func TestAccountBlockKeepsRowsThroughAFailedRefresh(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	v := accountstate.Facts{
		Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthOK,
		Obs: &accountstate.Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 42, Severity: "normal"}},
			ObservedAt: now - 30, Source: accountstate.SourceLive,
		},
		Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, HTTPCode: 429, NextEligibleAt: now + 180},
	}
	lines := p.accountBlock(v, now, 16)
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

// body is what a layout says about an account beyond naming it: the block
// without its header line (which always shows the launcher), the compact row
// whole (its name column is the label).
func body(b Board, layout Layout) string {
	g := b.Groups[0]
	if layout == LayoutBlocks {
		g = g[1:]
	}
	return strings.Join(g, "\n")
}

// A stale access token is routine housekeeping. It must never be phrased as
// expiry or send the user to /login — that was the false alarm users learned
// to distrust — in either layout.
func TestStaleTokenReadsAsHousekeeping(t *testing.T) {
	p := NewPalette(false)
	v := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthOK,
		Attempt: accountstate.Attempt{State: accountstate.AttemptTokenStale}}
	for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
		said := body(p.Board([]accountstate.Facts{v}, time.Now().Unix(), layout, 0), layout)
		if strings.Contains(said, "expired") || strings.Contains(said, "/login") {
			t.Errorf("layout %d: stale access token phrased as an account problem: %q", layout, said)
		}
		if !strings.Contains(said, "x-a") || !strings.Contains(said, "refreshes it") {
			t.Errorf("layout %d: remedy not stated: %q", layout, said)
		}
	}
}

// Old figures keep their severity colours in every column and layout, with
// staleness marked separately in the caption.
func TestStaleObservationIsMarked(t *testing.T) {
	p := NewPalette(true)
	now := time.Now().Unix()
	v := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthOK,
		Obs: &accountstate.Observation{
			Rows:       []usage.Row{session(12, now+3600), weeklyAll(58, now+86400), scoped("Fable", 97, now+86400)},
			ObservedAt: now - int64((22 * time.Hour).Seconds()), Source: accountstate.SourceCache,
		},
		Attempt: accountstate.Attempt{State: accountstate.AttemptDeferred, NextEligibleAt: now + 40},
	}
	for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
		joined := strings.Join(p.Board([]accountstate.Facts{v}, now, layout, 0).Groups[0], "\n")
		for _, want := range []struct{ color, pct string }{
			{p.Grn, "12%"}, {p.Yel, "58%"}, {p.Red, "97%"},
		} {
			// No reset or other escape may mask the severity before the percent.
			pattern := regexp.QuoteMeta(want.color) + `[^\x1b]*` + regexp.QuoteMeta(want.pct+p.Rst)
			if !regexp.MustCompile(pattern).MatchString(joined) {
				t.Errorf("layout %v: %s lost its severity colour: %q", layout, want.pct, joined)
			}
		}
		if !strings.Contains(joined, p.Yel+"stale"+p.Rst) || !strings.Contains(joined, "22h ago") {
			t.Errorf("layout %v: staleness not surfaced: %q", layout, joined)
		}
		if !strings.Contains(joined, "Claude Code's cache") {
			t.Errorf("layout %v: source not surfaced: %q", layout, joined)
		}
	}
	if v.Fresh(now) {
		t.Error("22h-old observation reported as fresh")
	}
}

// accountstate.Health must reach the user whether or not rows exist. An account that needs
// /login but happens to have a recent cache would otherwise render as ordinary
// coloured bars with no warning — and the picker would offer it as a live
// choice, which is precisely the class of mistake this model exists to stop.
// This is also the owner of the health words: "not logged in" is the one the
// pty harness reads (accounts_compact.exp).
func TestNonOKHealthSurfacesEvenWithRows(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	base := accountstate.Facts{
		Label: "a@x.com", Launcher: "x-a",
		Obs: &accountstate.Observation{
			Rows:       []usage.Row{{Label: "5h session", Percent: 12, Severity: "normal"}},
			ObservedAt: now - 5, Source: accountstate.SourceCache,
		},
		Attempt: accountstate.Attempt{State: accountstate.AttemptNone},
	}
	for _, c := range []struct {
		health accountstate.Health
		want   []string
	}{
		{accountstate.HealthNoLogin, []string{"not logged in", "/login"}},
		{accountstate.HealthReloginRequired, []string{"/login"}},
		{accountstate.HealthBadBlob, []string{"headroom check"}},
		{accountstate.HealthUnknown, []string{"headroom check"}},
	} {
		v := base
		v.Health = c.health
		joined := strings.Join(p.accountBlock(v, now, 16), "\n")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("health %v hidden behind cached rows; want %q in:\n%s", c.health, w, joined)
			}
		}
	}
}

// A row whose own reset instant has passed describes a window that has ended.
// The percent is a fact about the past, and a low one reads as headroom that
// may not exist — measured on the live machine as an 8% bar for a window that
// had rolled over 33 hours earlier. (That such an account is no live choice is
// accountstate's, in TestRolledOverWindowIsNotActionable.)
func TestRolledOverRowIsNotAnAnswer(t *testing.T) {
	now := time.Now().Unix()
	p := NewPalette(false)
	rolled := usage.Row{Label: "5h session", Percent: 8, ResetAt: now - 33*3600,
		PercentState: tag.OK, ResetState: tag.OK}

	line := p.limitRow(rolled, now, 12)
	if strings.Contains(line, "8%") {
		t.Errorf("a rolled-over window still shows its old percent: %q", line)
	}
	if !strings.Contains(line, "window rolled over") {
		t.Errorf("a rolled-over window must say so: %q", line)
	}
	if strings.Contains(line, "drift") {
		t.Errorf("rolling over is not drift — the field parsed: %q", line)
	}
}
