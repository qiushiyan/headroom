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

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// bare strips styling so a test can reason about cells and columns.
func bare(s string) string { return sgr.ReplaceAllString(s, "") }

func fresh(now int64, rows ...usage.Row) *accountstate.Observation {
	return &accountstate.Observation{Rows: rows, ObservedAt: now - 5, Source: accountstate.SourceLive}
}

func session(pct int, resetAt int64) usage.Row {
	return usage.Row{Label: "5h session", Kind: usage.KindSession, Group: "session", Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

func weeklyAll(pct int, resetAt int64) usage.Row {
	return usage.Row{Label: "All models (7d)", Kind: usage.KindWeeklyAll, Group: "weekly", Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

func scoped(model string, pct int, resetAt int64) usage.Row {
	return usage.Row{Label: model + " (7d)", Kind: usage.KindWeeklyScoped, Group: "weekly", Model: model, Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

// Columns are the union of decoded identities across accounts, ordered by
// how much they decide the choice — all models, then the 5h session, then
// the model-scoped weekly windows — however the rows arrived, keyed by identity
// and never by heading: two rows spelling the same label under different
// identities are two columns, an unknown kind is carried after the known
// ones, and a row whose identity failed the contract forms no column at all.
func TestCompactColumnsAreIdentityOrderedAndOpen(t *testing.T) {
	now := time.Now().Unix()
	views := []accountstate.Facts{
		{Label: "a@x.com", Obs: fresh(now, scoped("Fable", 71, now+90000), session(12, now+3600))},
		{Label: "b@x.com", Obs: fresh(now,
			usage.Row{Label: "mystery", Kind: "mystery", Group: "weekly", Percent: 5},
			weeklyAll(52, now+90000),
			usage.Row{Label: "5h session", Kind: "session", Group: "weekly", IdentityState: tag.Bad},
			scoped("Opus", 40, now+90000),
		)},
		{Label: "c@x.com", Obs: fresh(now,
			// Same heading as a's scoped column, different decoded identity.
			usage.Row{Label: "Fable (7d)", Kind: "weekly_scoped", Group: "weekly", Model: "Fable ", Percent: 1},
		)},
	}
	cols := columns(views)
	var got []string
	for _, c := range cols {
		got = append(got, c.id.Kind+"/"+c.id.Model)
	}
	want := []string{"weekly_all/", "session/", "weekly_scoped/Fable", "weekly_scoped/Fable ", "weekly_scoped/Opus", "mystery/"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	// The row with the bad identity contributes nothing: its account's
	// caption says drift instead.
	b := NewPalette(false).Board(views, now, LayoutCompact, 0)
	if !strings.Contains(b.Groups[1][0], "drift") {
		t.Errorf("bad-identity row left no drift caption: %q", b.Groups[1][0])
	}
}

// Each cell state has its own token, and the two layers keep their own
// colour: the percent takes the bar's severity with the bar's precedence (a
// percent that failed to parse is red, a rolled-over window is dim); the
// reset is dim beside it
// unless it is the reset itself that drifted.
func TestCompactCellStates(t *testing.T) {
	p := NewPalette(true)
	now := int64(1_800_000_000)
	cases := []struct {
		name string
		row  usage.Row
		want cell
	}{
		{"valid, low", session(12, now+2*3600+6*60), cell{"12%", "2.1h", p.Grn, p.Dim}},
		{"valid, high, days", weeklyAll(97, now+4*86400+20*3600), cell{"97%", "4.8d", p.Red, p.Dim}},
		{"valid, middling, minutes", session(55, now+35*60), cell{"55%", "0.6h", p.Yel, p.Dim}},
		{"reset legitimately absent", session(0, 0), cell{"0%", "—", p.Grn, p.Dim}},
		{"reset malformed is red", usage.Row{Kind: "session", Percent: 12, Severity: "normal", ResetState: tag.Bad}, cell{"12%", "reset?", p.Grn, p.Red}},
		{"rolled over is dim and not a number", session(12, now-60), cell{"?%", "rolled", p.Dim, p.Dim}},
		{"bad percent is red", usage.Row{Kind: "session", Severity: "normal", PercentState: tag.Bad, ResetAt: now + 3600}, cell{"?%", "1.0h", p.Red, p.Dim}},
		// Both states at once, each in its own layer: the percent is drift
		// (red), the window has ended (rolled) — never a negative clock.
		{"bad percent whose window has ended", usage.Row{Kind: "session", Severity: "normal", PercentState: tag.Bad, ResetAt: now - 1800}, cell{"?%", "rolled", p.Red, p.Dim}},
		{"severity overrides a low percent", usage.Row{Kind: "session", Percent: 3, Severity: "warning", ResetAt: now + 3600}, cell{"3%", "1.0h", p.Red, p.Dim}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c := p.cellFor(tc.row, now); c != tc.want {
				t.Fatalf("cell = %+v, want %+v", c, tc.want)
			}
		})
	}
}

// The two layers align in their own slots: every percent ends in the same
// column and every reset starts in the same column, whatever their lengths.
func TestCompactCellLayersAlign(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	views := []accountstate.Facts{
		{Label: "a@x.com", Obs: fresh(now, session(100, now+18*3600+40*60)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "b@x.com", Obs: fresh(now, session(0, 0)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "c@x.com", Obs: fresh(now, session(5, now+4*86400)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	b := p.Board(views, now, LayoutCompact, 0)
	pctEnd, resetStart := -1, -1
	for i, g := range b.Groups {
		line := bare(g[0])
		e := strings.Index(line, "%") + 1
		s := e + 1
		if i == 0 {
			pctEnd, resetStart = e, s
		}
		if e != pctEnd || s != resetStart || line[s] == ' ' {
			t.Errorf("row %d misaligned (percent ends %d, reset starts %d): %q", i, e, s, line)
		}
	}
}

// Two rows claiming one identity is something the vocabulary cannot carry;
// choosing either percent would present a guess as a figure.
func TestCompactDuplicateIdentityIsNotAFigure(t *testing.T) {
	now := time.Now().Unix()
	views := []accountstate.Facts{{Label: "a@x.com", Obs: fresh(now, session(12, now+3600), session(80, now+3600))}}
	line := bare(NewPalette(true).Board(views, now, LayoutCompact, 0).Groups[0][0])
	if !strings.Contains(line, "?%") || strings.Contains(line, "12%") || strings.Contains(line, "80%") {
		t.Fatalf("duplicate identity rendered a figure: %q", line)
	}
	if !strings.Contains(line, "drift") {
		t.Fatalf("duplicate identity left no drift caption: %q", line)
	}
}

// The caption is a composition of independent clauses, never one verdict,
// and it is the same clauses in both layouts — the block spells them over its
// lines, the compact row joins them at its end; a layout is only a layout.
// The traps: a single state per account has no cell for "current figures
// whose newest refresh was refused" or "current figures from Claude Code's
// cache", so both facts vanish behind bars that look freshly fetched; and a
// logged-out account holding a fresh cache is both unusable and
// well-observed, so a caption that said only one would hide the warning or
// where the figures came from. The ordinary case says nothing at all.
func TestCaptionKeepsEveryClause(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	obs := func(age time.Duration, src accountstate.Source, rows ...usage.Row) *accountstate.Observation {
		return &accountstate.Observation{Rows: rows, ObservedAt: now - int64(age.Seconds()), Source: src}
	}
	row := session(12, now+3600)
	attempt := func(s accountstate.AttemptState) accountstate.Attempt { return accountstate.Attempt{State: s} }
	cases := []struct {
		name    string
		view    accountstate.Facts
		want    []string
		notWant []string
		once    []string // said exactly once
		quiet   bool     // no caption at all: the block is its header and its bars
	}{
		{
			name:  "fresh and ours says nothing",
			view:  accountstate.Facts{Label: "a@x.com", Obs: obs(5*time.Second, accountstate.SourceLive, row), Attempt: attempt(accountstate.AttemptOK)},
			want:  []string{"12%"},
			quiet: true,
		},
		{
			name:  "current, replayed from our own store, still says nothing",
			view:  accountstate.Facts{Label: "a@x.com", Obs: obs(20*time.Second, accountstate.SourceStore, row), Attempt: attempt(accountstate.AttemptNone)},
			quiet: true,
		},
		{
			name:  "a deferred refresh over current figures is a non-event",
			view:  accountstate.Facts{Label: "a@x.com", Obs: obs(20*time.Second, accountstate.SourceStore, row), Attempt: attempt(accountstate.AttemptDeferred)},
			quiet: true,
		},
		{
			name: "health problem beside a fresh cache",
			view: accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthNoLogin,
				Obs: obs(5*time.Second, accountstate.SourceCache, row)},
			want: []string{"12%", "/login", "via Claude Code's cache"},
		},
		{
			name:    "current figures whose refresh was refused still report it",
			view:    accountstate.Facts{Label: "a@x.com", Obs: obs(20*time.Second, accountstate.SourceLive, row), Attempt: attempt(accountstate.AttemptRefused)},
			want:    []string{"observed 20s ago", "rate limited"},
			notWant: []string{"stale"},
		},
		{
			name:    "current figures from the vendor cache still name their source",
			view:    accountstate.Facts{Label: "a@x.com", Obs: obs(20*time.Second, accountstate.SourceCache, row), Attempt: attempt(accountstate.AttemptOK)},
			want:    []string{"via Claude Code's cache"},
			notWant: []string{"stale"},
		},
		{
			name:    "a refresh in flight shows over the figures it will replace",
			view:    accountstate.Facts{Label: "a@x.com", Obs: obs(20*time.Second, accountstate.SourceStore, row), Attempt: attempt(accountstate.AttemptPending)},
			want:    []string{"fetching"},
			notWant: []string{"stale"},
		},
		{
			name: "stale and refused",
			view: accountstate.Facts{Label: "a@x.com", Obs: obs(2*time.Hour, accountstate.SourceStore, row),
				Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, NextEligibleAt: now + 40}},
			want: []string{"stale", "observed 2h ago", "rate limited", "40s"},
		},
		{
			name: "old figures are nagged about, whoever fetched them — deferral included",
			view: accountstate.Facts{Label: "a@x.com", Obs: obs(22*time.Hour, accountstate.SourceStore, row), Attempt: attempt(accountstate.AttemptDeferred)},
			want: []string{"stale", "observed 22h ago", "live check deferred"},
		},
		{
			name: "headroom's own state failing is said alone over good figures",
			view: accountstate.Facts{Label: "a@x.com", Obs: obs(5*time.Second, accountstate.SourceLive, row),
				Attempt: accountstate.Attempt{State: accountstate.AttemptOK, StoreError: "disk full"}},
			want:    []string{"state file unavailable"},
			notWant: []string{"not checked"},
		},
		{
			name:    "nothing known, healthy",
			view:    accountstate.Facts{Label: "a@x.com", Health: accountstate.HealthOK, Attempt: attempt(accountstate.AttemptTransport)},
			want:    []string{"usage unknown", "fetch failed"},
			notWant: []string{"—  "}, // no dash cells: the caption is the row
		},
		{
			name: "an empty observation is a contractual answer, said once",
			view: accountstate.Facts{Label: "a@x.com", Obs: obs(5*time.Second, accountstate.SourceLive), Attempt: attempt(accountstate.AttemptNoLimits)},
			once: []string{"no limits reported"},
		},
		{
			name:    "nothing known, logged out",
			view:    accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthReloginRequired},
			want:    []string{"x-a", "/login"},
			notWant: []string{"usage unknown"},
		},
		{
			// The compact row's mark alone would send the user to the block
			// to learn which dir to fix.
			name: "a mismatched dir is named, not only marked",
			view: accountstate.Facts{Label: "b@x.com", DirMismatch: "c@x.com", Obs: obs(5*time.Second, accountstate.SourceLive, row), Attempt: attempt(accountstate.AttemptOK)},
			want: []string{"dir says c@x.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A second, ordinary account supplies the compact columns so the
			// case under test can be the one lacking them.
			other := accountstate.Facts{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: attempt(accountstate.AttemptOK)}
			for _, layout := range []Layout{LayoutBlocks, LayoutCompact} {
				group := p.Board([]accountstate.Facts{tc.view, other}, now, layout, 0).Groups[0]
				text := bare(strings.Join(group, "\n"))
				for _, w := range tc.want {
					if !strings.Contains(text, w) {
						t.Errorf("layout %d: caption lacks %q in %q", layout, w, text)
					}
				}
				for _, nw := range tc.notWant {
					if strings.Contains(text, nw) {
						t.Errorf("layout %d: caption carries %q in %q", layout, nw, text)
					}
				}
				for _, w := range tc.once {
					if n := strings.Count(text, w); n != 1 {
						t.Errorf("layout %d: %q said %d times in %q", layout, w, n, text)
					}
				}
				if !tc.quiet {
					continue
				}
				if strings.Contains(text, "observed") || strings.Contains(text, "·") {
					t.Errorf("layout %d: a caption on the ordinary case: %q", layout, text)
				}
				if layout == LayoutBlocks && len(group) != 1+len(tc.view.Obs.Rows) {
					t.Errorf("block is not its header and its bars: %q", group)
				}
			}
		})
	}
}

// A row with no figures starts its caption where the first cell would, so
// the health warning stands in the columns' space rather than after them.
func TestCompactCaptionStartsAtTheFirstColumnWhenThereAreNoRows(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	views := []accountstate.Facts{
		{Label: "a@x.com", Launcher: "x-a", Health: accountstate.HealthNoLogin},
		{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	b := p.Board(views, now, LayoutCompact, 0)
	warn, cellRow := bare(b.Groups[0][0]), bare(b.Groups[1][0])
	if strings.Index(warn, "not logged in") != strings.Index(cellRow, "  1% ") {
		t.Fatalf("caption column mismatch:\n%q\n%q", warn, cellRow)
	}
}

// Marks and the name's colour are what survive a clipped caption: the
// current account carries ●, a dir mismatch carries a red !, and a health
// problem or a vendor block turns the name red so green figures never sit
// beside an unusable account with nothing else to say so.
func TestCompactRowMarksAndHealthColour(t *testing.T) {
	p := NewPalette(true)
	now := time.Now().Unix()
	blocked := fresh(now, session(1, now+3600))
	blocked.Allowance = usage.Allowance{State: usage.AllowanceBlocked}
	views := []accountstate.Facts{
		{Label: "a@x.com", Current: true, Obs: fresh(now, session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "b@x.com", DirMismatch: "c@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "d@x.com", Launcher: "x-d", Health: accountstate.HealthBadBlob, Obs: fresh(now, session(1, now+3600))},
		{Label: "e@x.com", Obs: blocked, Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	g := p.Board(views, now, LayoutCompact, 0).Groups
	if !strings.Contains(g[0][0], p.Bold+"●"+p.Rst) {
		t.Errorf("current mark missing: %q", g[0][0])
	}
	if !strings.Contains(g[1][0], p.Red+"!"+p.Rst) {
		t.Errorf("dir mismatch mark missing: %q", g[1][0])
	}
	if !strings.HasPrefix(g[2][0], p.Red+"d@x.com") {
		t.Errorf("unhealthy account's name not red: %q", g[2][0])
	}
	if !strings.HasPrefix(g[3][0], p.Red+"e@x.com") {
		t.Errorf("blocked account's name not red: %q", g[3][0])
	}
	if !strings.HasPrefix(g[0][0], p.Bold+"a@x.com") {
		t.Errorf("healthy account's name not bold: %q", g[0][0])
	}
}

// The name column gives way before the caption does. On a wide terminal
// (or a pipe) the longest label sets it, up to the cap; on a narrow one it
// shrinks toward the floor so the caption keeps its reserve.
func TestCompactNameColumnGivesWayToTheCaption(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	long := "someone.with.a.long.address@example.com" // 39 cells
	views := []accountstate.Facts{
		{Label: long, Obs: fresh(now, session(1, now+3600), weeklyAll(50, now+90000), scoped("Fable", 70, now+90000)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	nameCells := func(width int) int {
		// The long label fills its column exactly (ellipsized when capped)
		// and carries no spaces, so its first field is the column.
		row := bare(p.Board(views, now, LayoutCompact, width).Groups[0][0])
		return Cells(strings.Fields(row)[0])
	}
	if got := nameCells(0); got != nameCap {
		t.Errorf("unconstrained name column = %d cells, want the cap %d", got, nameCap)
	}
	if got := nameCells(200); got != nameCap {
		t.Errorf("wide terminal name column = %d cells, want the cap %d", got, nameCap)
	}
	if got := nameCells(80); got >= nameCap || got < nameFloor {
		t.Errorf("80-column name column = %d cells, want between the floor %d and the cap %d", got, nameFloor, nameCap)
	}
	if got := nameCells(40); got != nameFloor {
		t.Errorf("tiny terminal name column = %d cells, want the floor %d", got, nameFloor)
	}
	row := bare(p.Board(views, now, LayoutCompact, 80).Groups[0][0])
	if !strings.Contains(row, "…") || strings.Contains(row, long) {
		t.Errorf("long label not ellipsized at 80 columns: %q", row)
	}
}

// Every rendered line is one physical row: vendor text with a newline or a
// control byte in it would otherwise split the frame the in-place redraw
// counts on, and a wide script is measured in cells so the columns align.
func TestCompactRowsAreOnePhysicalRowEach(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	views := []accountstate.Facts{
		{Label: "evil\nname@x.com", Obs: fresh(now, session(1, now+3600), usage.Row{Label: "Fa\x1bble (7d)", Kind: "weekly_scoped", Group: "weekly", Model: "Fa\x1bble", Percent: 1, ResetAt: now + 3600}), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
		{Label: "日本語@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	b := p.Board(views, now, LayoutCompact, 0)
	for _, line := range append(b.Header, b.Groups[0][0], b.Groups[1][0]) {
		if strings.ContainsAny(line, "\n\r\t") || strings.Contains(bare(line), "\x1b") {
			t.Errorf("line carries a control byte: %q", line)
		}
	}
	// The wide label pads by cells, so the session cell — the first figure
	// on both rows — starts in the same column.
	a, c := bare(b.Groups[0][0]), bare(b.Groups[1][0])
	if Cells(a[:strings.Index(a, "1% 1.0h")]) != Cells(c[:strings.Index(c, "1% 1.0h")]) {
		t.Errorf("cells misaligned across a wide label:\n%q\n%q", a, c)
	}
}

// One spelling of time-to-reset for both layouts: tenths of an hour under a
// day, tenths of a day from a day up, the unit turning on the rounded value.
func TestRemaining(t *testing.T) {
	cases := map[int64]string{
		4*86400 + 20*3600 + 5*60: "4.8d",
		2*3600 + 6*60:            "2.1h",
		35*60 + 59:               "0.6h",
		30:                       "0.0h",
		23*3600 + 58*60:          "1.0d", // rounds to 24.0h, so it is a day
		23*3600 + 56*60:          "23.9h",
		86400:                    "1.0d",
	}
	for rem, want := range cases {
		if got := Remaining(rem); got != want {
			t.Errorf("Remaining(%ds) = %q, want %q", rem, got, want)
		}
	}
}

// The caption is clipped from the right on a narrow terminal, so its order
// is what survives: the clauses that change what the user does (stale, how
// the refresh went) come before the ones that explain (how old, from where).
// The trap this pins: a refused refresh on a stale cached observation sat
// behind "observed 2h ago · via Claude Code's cache" and was the first thing
// a 110-column terminal cut.
func TestCompactCaptionOrdersSignalsBeforeExplanations(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	refused := accountstate.Facts{Label: "qiushi.yann@gmail.com", Launcher: "x-q",
		Obs:     &accountstate.Observation{Rows: []usage.Row{scoped("Fable", 83, now+90000), session(12, now+3600), weeklyAll(42, now+90000)}, ObservedAt: now - 7200, Source: accountstate.SourceCache},
		Attempt: accountstate.Attempt{State: accountstate.AttemptRefused, NextEligibleAt: now + 40}}
	// A second account with a window of its own adds a fourth column.
	other := accountstate.Facts{Label: "b@x.com", Obs: fresh(now, scoped("Opus", 1, now+3600), session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}}
	row := bare(p.Board([]accountstate.Facts{refused, other}, now, LayoutCompact, 0).Groups[0][0])
	order := []string{"stale", "rate limited", "observed 2h ago", "via Claude Code's cache"}
	last := -1
	for _, w := range order {
		i := strings.Index(row, w)
		if i < 0 || i < last {
			t.Fatalf("caption clauses out of order (want %v): %q", order, row)
		}
		last = i
	}
	clipped := bare(Clip(p.Board([]accountstate.Facts{refused, other}, now, LayoutCompact, 110).Groups[0][0], 110))
	if !strings.Contains(clipped, "rate limited") {
		t.Errorf("110 columns lost the refusal: %q", clipped)
	}
}

// Under width pressure the row gives way in cost order: heading surplus
// (padding) first, then the name toward its floor, and only then the
// caption's reserve. With a fourth window on another account an 80-column
// terminal must still show this account's "stale".
func TestCompactHeadingsGiveWayBeforeNames(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	views := []accountstate.Facts{
		{Label: "someone.with.a.long.address@example.com",
			Obs:     &accountstate.Observation{Rows: []usage.Row{scoped("Fable", 83, now+90000), session(12, now+3600), weeklyAll(42, now+90000)}, ObservedAt: now - 7200, Source: accountstate.SourceStore},
			Attempt: accountstate.Attempt{State: accountstate.AttemptNone}},
		{Label: "b@x.com", Obs: fresh(now, scoped("Opus", 1, now+3600), session(1, now+3600)), Attempt: accountstate.Attempt{State: accountstate.AttemptOK}},
	}
	wide := bare(p.Board(views, now, LayoutCompact, 200).Header[0])
	if !strings.Contains(wide, "All models (7d)") {
		t.Fatalf("headings shortened with room to spare: %q", wide)
	}
	b := p.Board(views, now, LayoutCompact, 80)
	if h := bare(b.Header[0]); !strings.Contains(h, "…") || strings.Contains(h, "All models (7d)") {
		t.Errorf("headings kept their surplus under pressure: %q", h)
	}
	row := bare(b.Groups[0][0])
	if n := Cells(strings.Fields(row)[0]); n < nameFloor {
		t.Errorf("name squeezed below the floor to %d: %q", n, row)
	}
	if clipped := bare(Clip(b.Groups[0][0], 80)); !strings.Contains(clipped, "stale") {
		t.Errorf("80 columns lost the staleness signal: %q", clipped)
	}
}
