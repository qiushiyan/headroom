package render

// The board as one rendering: every account's view, in one of two layouts,
// assembled here and nowhere else. The picker and the one-shot print both
// consume the result — the picker adds a selection mark and windows the
// groups to the screen, the print writes them out — and neither computes a
// column, a width or a caption of its own. Two callers each assembling a
// table is how a header and its rows come to disagree; one function that
// returns both is how they cannot.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Layout is a presentation of the same views, never a different document:
// `--json` and `limits` serialize the data the block and the row both draw.
type Layout int

const (
	// LayoutBlocks is the classic block per account: a header line, a health
	// line when something is wrong, one bar per limit, a provenance caption.
	LayoutBlocks Layout = iota
	// LayoutCompact is one row per account under a column header: the label,
	// one cell per limit window (percent and a compact time-to-reset), and
	// every applicable caption clause at the end of the row.
	LayoutCompact
)

// Board is a rendered board before selection and windowing: Header is chrome
// the picker keeps above the scrolled body — the legend, and the compact
// layout's column headings — and Groups holds one line group per account in
// the views' order. The one-shot print and the picker both draw Header, then
// Body.
type Board struct {
	Header []string
	Groups [][]string
	blocks bool
}

// Body is the accounts' lines in order, a blank line between two blocks.
func (b Board) Body() []string {
	var lines []string
	for i, g := range b.Groups {
		if i > 0 && b.blocks {
			lines = append(lines, "")
		}
		lines = append(lines, g...)
	}
	return lines
}

// Board renders every account in one layout. width is the terminal's cell
// width and bounds the compact name column so the caption keeps room; 0
// means unconstrained (a pipe). Lines are not clipped here — the caller owns
// the terminal and clips to it — but every line is one physical row of
// sanitized text, which is what the in-place redraw needs.
func (p Palette) Board(views []accountstate.Facts, now int64, layout Layout, width int) Board {
	if layout == LayoutCompact {
		b := p.compactBoard(views, now, width)
		// The compact row's bare → needs saying; the block's "← next" says
		// itself.
		b.Header = append([]string{p.Dim + "→ marks the account the next launch takes" + p.Rst}, b.Header...)
		return b
	}
	lw := labelWidth(views)
	b := Board{Groups: make([][]string, len(views)), blocks: true}
	for i, v := range views {
		b.Groups[i] = p.accountBlock(v, now, lw)
	}
	return b
}

const (
	// nameCap is the widest the compact name column grows; an email longer
	// than this is ellipsized. nameFloor is the narrowest a tight terminal
	// can squeeze it to — below that a label stops identifying anything.
	nameCap   = 24
	nameFloor = 10
	// captionReserve is the room a compact row keeps for its caption before
	// the name column starts giving way: enough for "not logged in — run
	// x-alice and /login" to keep its verb on an 80-column terminal.
	captionReserve = 24
	// selectionPrefix is the picker's mark column ("▶ "), reserved in the
	// width arithmetic whether or not the caller draws one.
	selectionPrefix = 2
	// marksWidth holds the next mark and the dir-mismatch mark.
	marksWidth = 2
	colGap     = "  "
)

// A column is one limit window across every account, identified by the
// vendor's decoded vocabulary — kind, group and the scoped model, and for
// Codex the feature and the window's duration too (two accounts whose
// `primary` slots run for different lengths are different limits) — and
// never by the heading derived from it. Columns run in usage.CompareRows'
// order, which is the vendor's to decide; pages never share a table, so one
// vendor's order is all a board needs. A row whose identity failed the
// contract forms no column, because a limit that cannot say which limit it is
// has no place to be compared in.
type column struct {
	id         usage.Row // the identity every row under it shares; no figure
	label      string
	width      int
	resetWidth int // widest reset token in the column
}

func (c column) matches(r usage.Row) bool {
	return c.id.Kind == r.Kind && c.id.Group == r.Group && c.id.Model == r.Model &&
		c.id.Feature == r.Feature && c.id.WindowSeconds == r.WindowSeconds
}

func columns(views []accountstate.Facts) []column {
	var cols []column
	for _, v := range views {
		if v.Obs == nil {
			continue
		}
		for _, r := range v.Obs.Rows {
			if r.IdentityState == tag.Bad {
				continue
			}
			if slices.ContainsFunc(cols, func(c column) bool { return c.matches(r) }) {
				continue
			}
			id := usage.Row{Kind: r.Kind, Group: r.Group, Model: r.Model,
				Feature: r.Feature, WindowSeconds: r.WindowSeconds}
			cols = append(cols, column{id: id, label: Sanitize(r.Label)})
		}
	}
	if len(cols) > 0 {
		vendor := views[0].Vendor // a board is one page, so one vendor
		slices.SortStableFunc(cols, func(a, b column) int { return usage.CompareRows(vendor, a.id, b.id) })
	}
	return cols
}

// A cell is one limit's figure in one row, in two layers: the percent,
// which carries the severity colour and is what the eye is meant to land
// on, and the reset beside it, dim, secondary. Each part aligns in its own
// slot — the percent right-aligned to four cells, the reset left-aligned to
// the column's widest token — so a row of cells reads as a column of
// numbers with clocks after them, not as a ragged pair. Text is plain so
// widths can be measured; colour wraps it last.
type cell struct {
	pct, reset           string
	pctColor, resetColor string
}

// pctWidth is the percent slot: "100%" and "  ?%" both fit.
const pctWidth = 4

// cellFor is the compact counterpart of limitRow: the same rowState, spelled
// as distinct tokens rather than a legend — a valid future reset, a reset
// legitimately absent (a 5h window nobody has opened), a reset that failed
// to parse, a window nobody has started, a window that has since ended, and
// a percent that is not a number. The percent's colour is severity's, shared
// with the bar; the reset is dim except when it is the thing that drifted.
func (p Palette) cellFor(r usage.Row, now int64) cell {
	pct, reset := rowState(r, now)
	c := cell{pct: fmt.Sprintf("%d%%", r.Percent), pctColor: p.severity(r, pct), resetColor: p.Dim}
	if pct != pctFigure {
		c.pct = "?%"
	}
	switch reset {
	case resetAt:
		c.reset = Remaining(r.ResetAt - now)
	case resetAbsent:
		c.reset = "—"
	case resetUnparseable:
		c.reset, c.resetColor = "reset?", p.Red
	case resetUnstarted:
		c.reset = "not started"
	case resetRolled:
		c.reset = "rolled"
	}
	return c
}

// caption is every clause the block says about this account, joined on one
// line in clip order — the clauses' own. Never one winner: a logged-out
// account holding a fresh cache is both of those things, and a row that
// picked one would be the axis collapse this model exists to prevent. The
// mismatched dir is named here as well as marked, since the mark alone would
// send the user to the block to learn which dir to fix.
func (p Palette) caption(v accountstate.Facts, now int64, drift bool) string {
	c := p.clauses(v, now, drift)
	var all [clauseKinds]clauseKind
	for k := range all {
		all[k] = clauseKind(k)
	}
	return p.join(c, all[:]...)
}

// compactRow is one account's row before styling: its cells in column order
// (nil when the account has no rows at all — the caption then starts where
// the cells would), and whether anything about it drifted.
type compactRow struct {
	cells []cell
	drift bool
}

func (p Palette) compactBoard(views []accountstate.Facts, now int64, width int) Board {
	cols := columns(views)
	rows := make([]compactRow, len(views))
	for i, v := range views {
		if v.Obs == nil || len(v.Obs.Rows) == 0 {
			continue
		}
		row := compactRow{cells: make([]cell, len(cols))}
		for j, c := range cols {
			var matched []usage.Row
			for _, r := range v.Obs.Rows {
				if c.matches(r) {
					matched = append(matched, r)
				}
			}
			switch len(matched) {
			case 0:
				row.cells[j] = cell{pct: "—", pctColor: p.Dim, resetColor: p.Dim}
			case 1:
				row.cells[j] = p.cellFor(matched[0], now)
			default:
				// Two rows claiming one identity: the vendor has said
				// something this vocabulary cannot carry. Choosing either
				// percent would present a guess as a figure.
				row.cells[j] = cell{pct: "?%", pctColor: p.Red, resetColor: p.Dim}
				row.drift = true
			}
		}
		for _, r := range v.Obs.Rows {
			if r.Drifted() {
				row.drift = true
			}
		}
		rows[i] = row
	}

	// The reset slot is the column's widest token so every clock starts in
	// the same place under the same heading; column and name widths are
	// then budgeted against the terminal together.
	for j := range cols {
		for _, row := range rows {
			if row.cells != nil {
				cols[j].resetWidth = max(cols[j].resetWidth, Cells(row.cells[j].reset))
			}
		}
	}
	nameW := budget(views, cols, width)

	// Header: the name column's heading, blank marks, then every column's
	// label. Dim throughout — it is chrome, not data.
	var h strings.Builder
	h.WriteString(p.Dim + PadCell("account", nameW) + " " + strings.Repeat(" ", marksWidth))
	for _, c := range cols {
		h.WriteString(colGap + PadCell(c.label, c.width))
	}
	h.WriteString(p.Rst)

	b := Board{Header: []string{strings.TrimRight(h.String(), " ")}, Groups: make([][]string, len(views))}
	for i, v := range views {
		b.Groups[i] = []string{p.compactLine(v, rows[i], cols, nameW, now)}
	}
	return b
}

// budget sizes the columns and the name against the terminal and returns
// the name width. Unconstrained, every heading stands at full width and the
// name column is the longest label up to its cap. Under pressure the row
// gives way in the order of what it costs to lose: heading surplus first
// (a heading wider than its cells is padding; it ellipsizes to the cells),
// then the name down to its floor — and only then does the caption, the
// clip casualty, lose its reserve. Spending the caption's room on padding
// is how a fourth window on one account hid another account's warning.
func budget(views []accountstate.Facts, cols []column, width int) int {
	longest := 0
	for _, v := range views {
		longest = max(longest, Cells(Sanitize(v.Label)))
	}
	nameW := max(min(longest, nameCap), 1)
	fixed := func() int {
		n := selectionPrefix + 1 + marksWidth
		for _, c := range cols {
			n += len(colGap) + c.width
		}
		return n
	}
	for j := range cols {
		cols[j].width = max(Cells(cols[j].label), pctWidth+1+cols[j].resetWidth)
	}
	if width <= 0 || width-fixed()-captionReserve >= nameW {
		return nameW
	}
	for j := range cols {
		cols[j].width = pctWidth + 1 + cols[j].resetWidth
	}
	if avail := width - fixed() - captionReserve; avail < nameW {
		nameW = max(avail, nameFloor)
	}
	return nameW
}

// compactLine draws one row: name, marks, cells or caption. The name goes
// red on a health problem so the row's worst news survives even when the
// caption that spells it out is clipped off a narrow terminal — green
// figures beside an unusable account is the reading a clip must not produce.
func (p Palette) compactLine(v accountstate.Facts, row compactRow, cols []column, nameW int, now int64) string {
	nameColor := p.Bold
	if healthText(v) != "" || v.Blocked() {
		nameColor = p.Red
	}
	var b strings.Builder
	b.WriteString(nameColor + PadCell(Sanitize(v.Label), nameW) + p.Rst + " ")
	if v.Next {
		b.WriteString(p.Bold + "→" + p.Rst)
	} else {
		b.WriteString(" ")
	}
	if v.DirMismatch != "" {
		b.WriteString(p.Red + "!" + p.Rst)
	} else {
		b.WriteString(" ")
	}
	capt := p.caption(v, now, row.drift)
	if row.cells == nil {
		// No figures at all: the caption is the row, starting where the
		// first cell would, so it survives whatever width the cells would
		// have been clipped at.
		if capt != "" {
			b.WriteString(colGap + capt)
		}
		return b.String()
	}
	for j, c := range row.cells {
		last := capt == "" && j == len(row.cells)-1
		b.WriteString(colGap + c.pctColor + padLeft(c.pct, pctWidth) + p.Rst)
		if c.reset == "" && last {
			continue
		}
		reset := c.reset
		if !last {
			// The reset slot, then whatever the heading is wider by.
			reset = PadCell(reset, cols[j].width-pctWidth-1)
		}
		b.WriteString(" " + c.resetColor + reset + p.Rst)
	}
	if capt != "" {
		b.WriteString(colGap + capt)
	}
	return b.String()
}

// padLeft right-aligns plain text in a slot of the given cell width.
func padLeft(s string, width int) string {
	if n := Cells(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}
