package render

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/usage"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// bare strips styling so a test can reason about cells and columns.
func bare(s string) string { return sgr.ReplaceAllString(s, "") }

func fresh(now int64, rows ...usage.Row) *Observation {
	return &Observation{Rows: rows, ObservedAt: now - 5, Source: SourceLive}
}

func session(pct int, resetAt int64) usage.Row {
	return usage.Row{Label: "5h session", Kind: "session", Group: "session", Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

func weeklyAll(pct int, resetAt int64) usage.Row {
	return usage.Row{Label: "All models (7d)", Kind: "weekly_all", Group: "weekly", Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

func scoped(model string, pct int, resetAt int64) usage.Row {
	return usage.Row{Label: model + " (7d)", Kind: "weekly_scoped", Group: "weekly", Model: model, Percent: pct, ResetAt: resetAt, Severity: "normal"}
}

// The block layout through Board is the block layout: same lines, same
// label width, so the picker's switch to the shared entry point changed no
// pixel of the default.
func TestBoardBlocksIsAccountBlock(t *testing.T) {
	p := NewPalette(true)
	now := time.Now().Unix()
	views := []AccountView{
		{Label: "a@x.com", Launcher: "x-a", Current: true, Obs: fresh(now, session(12, now+3600), weeklyAll(52, now+90000)), Attempt: Attempt{State: AttemptOK}},
		{Label: "b@x.com", Launcher: "x-b", Health: HealthNoLogin},
	}
	b := p.Board(views, now, LayoutBlocks, 80)
	if b.Header != nil {
		t.Fatalf("block layout grew a header: %q", b.Header)
	}
	lw := LabelWidth(views)
	for i, v := range views {
		want := strings.Join(p.AccountBlock(v, now, lw), "\n")
		if got := strings.Join(b.Groups[i], "\n"); got != want {
			t.Errorf("group %d differs from AccountBlock:\n got %q\nwant %q", i, got, want)
		}
	}
}

// Columns are the union of decoded identities across accounts, ordered by
// how much they decide the choice — model-scoped weekly windows, then the
// 5h session, then all models — however the rows arrived, keyed by identity
// and never by heading: two rows spelling the same label under different
// identities are two columns, an unknown kind is carried after the known
// ones, and a row whose identity failed the contract forms no column at all.
func TestCompactColumnsAreIdentityOrderedAndOpen(t *testing.T) {
	now := time.Now().Unix()
	views := []AccountView{
		{Label: "a@x.com", Obs: fresh(now, scoped("Fable", 71, now+90000), session(12, now+3600))},
		{Label: "b@x.com", Obs: fresh(now,
			usage.Row{Label: "mystery", Kind: "mystery", Group: "weekly", Percent: 5},
			weeklyAll(52, now+90000),
			usage.Row{Label: "5h session", Kind: "session", Group: "weekly", IdentityState: usage.StateBad},
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
		got = append(got, c.kind+"/"+c.model)
	}
	want := []string{"weekly_scoped/Fable", "weekly_scoped/Fable ", "weekly_scoped/Opus", "session/", "weekly_all/", "mystery/"}
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
// stale figure loses its colour, a percent that failed to parse is red even
// when stale, a rolled-over window is dim); the reset is dim beside it
// unless it is the reset itself that drifted.
func TestCompactCellStates(t *testing.T) {
	p := NewPalette(true)
	now := int64(1_800_000_000)
	cases := []struct {
		name  string
		row   usage.Row
		stale bool
		want  cell
	}{
		{"valid, low", session(12, now+2*3600+4*60), false, cell{"12%", "2h04m", p.Grn, p.Dim}},
		{"valid, high, days", weeklyAll(97, now+4*86400+20*3600), false, cell{"97%", "4d20h", p.Red, p.Dim}},
		{"valid, middling, minutes", session(55, now+35*60), false, cell{"55%", "35m", p.Yel, p.Dim}},
		{"stale loses colour", weeklyAll(97, now+86400), true, cell{"97%", "1d0h", p.Dim, p.Dim}},
		{"reset legitimately absent", session(0, 0), false, cell{"0%", "—", p.Grn, p.Dim}},
		{"reset malformed is red", usage.Row{Kind: "session", Percent: 12, Severity: "normal", ResetState: usage.StateBad}, false, cell{"12%", "reset?", p.Grn, p.Red}},
		{"rolled over is dim and not a number", session(12, now-60), false, cell{"?%", "rolled", p.Dim, p.Dim}},
		{"bad percent is red", usage.Row{Kind: "session", Severity: "normal", PercentState: usage.StateBad, ResetAt: now + 3600}, false, cell{"?%", "1h00m", p.Red, p.Dim}},
		{"bad percent stays red when stale", usage.Row{Kind: "session", Severity: "normal", PercentState: usage.StateBad, ResetAt: now + 3600}, true, cell{"?%", "1h00m", p.Red, p.Dim}},
		{"severity overrides a low percent", usage.Row{Kind: "session", Percent: 3, Severity: "warning", ResetAt: now + 3600}, false, cell{"3%", "1h00m", p.Red, p.Dim}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c := p.cellFor(tc.row, now, tc.stale); c != tc.want {
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
	views := []AccountView{
		{Label: "a@x.com", Obs: fresh(now, session(100, now+18*3600+40*60)), Attempt: Attempt{State: AttemptOK}},
		{Label: "b@x.com", Obs: fresh(now, session(0, 0)), Attempt: Attempt{State: AttemptOK}},
		{Label: "c@x.com", Obs: fresh(now, session(5, now+4*86400)), Attempt: Attempt{State: AttemptOK}},
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
	views := []AccountView{{Label: "a@x.com", Obs: fresh(now, session(12, now+3600), session(80, now+3600))}}
	line := bare(NewPalette(true).Board(views, now, LayoutCompact, 0).Groups[0][0])
	if !strings.Contains(line, "?%") || strings.Contains(line, "12%") || strings.Contains(line, "80%") {
		t.Fatalf("duplicate identity rendered a figure: %q", line)
	}
	if !strings.Contains(line, "drift") {
		t.Fatalf("duplicate identity left no drift caption: %q", line)
	}
}

// The caption is every applicable clause, never one winner. The trap is the
// logged-out account holding a fresh cache: it is both unusable and
// well-observed, and a row that said only one would either hide the warning
// or hide where the figures came from.
func TestCompactCaptionKeepsEveryClause(t *testing.T) {
	p := NewPalette(false)
	now := time.Now().Unix()
	cases := []struct {
		name    string
		view    AccountView
		want    []string
		notWant []string
	}{
		{
			name: "health problem beside a fresh cache",
			view: AccountView{Label: "a@x.com", Launcher: "x-a", Health: HealthNoLogin,
				Obs: &Observation{Rows: []usage.Row{session(12, now+3600)}, ObservedAt: now - 5, Source: SourceCache}},
			want: []string{"12%", "/login", "via Claude Code's cache"},
		},
		{
			name: "stale and refused",
			view: AccountView{Label: "a@x.com", Obs: &Observation{Rows: []usage.Row{session(12, now+3600)}, ObservedAt: now - 7200, Source: SourceStore},
				Attempt: Attempt{State: AttemptRefused, NextEligibleAt: now + 40}},
			want: []string{"stale", "observed 2h ago", "rate limited; next attempt in 40s"},
		},
		{
			name:    "fresh and ours says nothing",
			view:    AccountView{Label: "a@x.com", Obs: fresh(now, session(12, now+3600)), Attempt: Attempt{State: AttemptOK}},
			want:    []string{"12% "},
			notWant: []string{"observed", "·"},
		},
		{
			name:    "nothing known, healthy",
			view:    AccountView{Label: "a@x.com", Health: HealthOK, Attempt: Attempt{State: AttemptTransport}},
			want:    []string{"usage unknown — fetch failed (network?)"},
			notWant: []string{"—  "}, // no dash cells: the caption is the row
		},
		{
			name: "an empty observation is a contractual answer",
			view: AccountView{Label: "a@x.com", Obs: &Observation{Rows: nil, ObservedAt: now - 5, Source: SourceLive}, Attempt: Attempt{State: AttemptNoLimits}},
			want: []string{"no limits reported"},
		},
		{
			name:    "nothing known, logged out",
			view:    AccountView{Label: "a@x.com", Launcher: "x-a", Health: HealthReloginRequired},
			want:    []string{"login expired — run x-a and /login"},
			notWant: []string{"usage unknown"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A second, ordinary account supplies the columns so the case
			// under test can be the one lacking them.
			other := AccountView{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}}
			line := bare(p.Board([]AccountView{tc.view, other}, now, LayoutCompact, 0).Groups[0][0])
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("caption lacks %q in %q", w, line)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(line, nw) {
					t.Errorf("caption carries %q in %q", nw, line)
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
	views := []AccountView{
		{Label: "a@x.com", Launcher: "x-a", Health: HealthNoLogin},
		{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}},
	}
	b := p.Board(views, now, LayoutCompact, 0)
	warn, cellRow := bare(b.Groups[0][0]), bare(b.Groups[1][0])
	if strings.Index(warn, "not logged in") != strings.Index(cellRow, "  1% ") {
		t.Fatalf("caption column mismatch:\n%q\n%q", warn, cellRow)
	}
}

// Marks and the name's colour are what survive a clipped caption: the
// current account carries ●, a dir mismatch carries a red !, and a health
// problem turns the name red so green figures never sit beside an unusable
// account with nothing else to say so.
func TestCompactRowMarksAndHealthColour(t *testing.T) {
	p := NewPalette(true)
	now := time.Now().Unix()
	views := []AccountView{
		{Label: "a@x.com", Current: true, Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}},
		{Label: "b@x.com", DirMismatch: "c@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}},
		{Label: "d@x.com", Launcher: "x-d", Health: HealthBadBlob, Obs: fresh(now, session(1, now+3600))},
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
	views := []AccountView{
		{Label: long, Obs: fresh(now, session(1, now+3600), weeklyAll(50, now+90000), scoped("Fable", 70, now+90000)), Attempt: Attempt{State: AttemptOK}},
		{Label: "b@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}},
	}
	nameCells := func(width int) int {
		h := bare(p.Board(views, now, LayoutCompact, width).Header[0])
		// The first heading is the model-scoped window's.
		return strings.Index(h, "Fable (7d)") - 1 - marksWidth - len(colGap)
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
	views := []AccountView{
		{Label: "evil\nname@x.com", Obs: fresh(now, session(1, now+3600), usage.Row{Label: "Fa\x1bble (7d)", Kind: "weekly_scoped", Group: "weekly", Model: "Fa\x1bble", Percent: 1, ResetAt: now + 3600}), Attempt: Attempt{State: AttemptOK}},
		{Label: "日本語@x.com", Obs: fresh(now, session(1, now+3600)), Attempt: Attempt{State: AttemptOK}},
	}
	b := p.Board(views, now, LayoutCompact, 0)
	for _, line := range append(b.Header, b.Groups[0][0], b.Groups[1][0]) {
		if strings.ContainsAny(line, "\n\r\t") || strings.Contains(bare(line), "\x1b") {
			t.Errorf("line carries a control byte: %q", line)
		}
	}
	// The wide label pads by cells, so the session cell — the last figure
	// on both rows — starts in the same column.
	a, c := bare(b.Groups[0][0]), bare(b.Groups[1][0])
	if Cells(a[:strings.LastIndex(a, "1% 1h00m")]) != Cells(c[:strings.LastIndex(c, "1% 1h00m")]) {
		t.Errorf("cells misaligned across a wide label:\n%q\n%q", a, c)
	}
}

func TestCompactReset(t *testing.T) {
	now := int64(1_800_000_000)
	cases := map[int64]string{
		4*86400 + 20*3600 + 5*60: "4d20h",
		2*3600 + 4*60:            "2h04m",
		35*60 + 59:               "35m",
		30:                       "0m",
	}
	for rem, want := range cases {
		if got := compactReset(now+rem, now); got != want {
			t.Errorf("compactReset(+%ds) = %q, want %q", rem, got, want)
		}
	}
}
