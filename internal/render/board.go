package render

// The board as one rendering: every account's view, in one of two layouts,
// assembled here and nowhere else. The picker and the one-shot print both
// consume the result — the picker adds a selection mark and windows the
// groups to the screen, the print writes them out — and neither computes a
// column, a width or a caption of its own. Two callers each assembling a
// table is how a header and its rows come to disagree; one function that
// returns both is how they cannot.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

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
// the picker keeps above the scrolled body (nil in the block layout), Groups
// holds one line group per account in the views' order.
type Board struct {
	Header []string
	Groups [][]string
}

// Board renders every account in one layout. width is the terminal's cell
// width and bounds the compact name column so the caption keeps room; 0
// means unconstrained (a pipe). Lines are not clipped here — the caller owns
// the terminal and clips to it — but every line is one physical row of
// sanitized text, which is what the in-place redraw needs.
func (p Palette) Board(views []AccountView, now int64, layout Layout, width int) Board {
	if layout == LayoutCompact {
		return p.compactBoard(views, now, width)
	}
	lw := LabelWidth(views)
	b := Board{Groups: make([][]string, len(views))}
	for i, v := range views {
		b.Groups[i] = p.AccountBlock(v, now, lw)
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
	// marksWidth holds the current mark and the dir-mismatch mark.
	marksWidth = 2
	colGap     = "  "
)

// A column is one limit window across every account, identified by the
// vendor's decoded vocabulary — kind, group and the scoped model — and never
// by the heading derived from it. Known kinds sort by how much they decide
// the choice: the model-scoped weekly windows first (the one that runs out),
// then the 5h session, then all models; unknown vocabulary follows
// deterministically. A row whose identity failed the contract forms no
// column, because a limit that cannot say which limit it is has no place to
// be compared in.
type column struct {
	kind, group, model string
	label              string
	width              int
	resetWidth         int // widest reset token in the column
}

func (c column) matches(r usage.Row) bool {
	return c.kind == r.Kind && c.group == r.Group && c.model == r.Model
}

func columnRank(kind string) int {
	switch kind {
	case "weekly_scoped":
		return 0
	case "session":
		return 1
	case "weekly_all":
		return 2
	default:
		return 3
	}
}

func columns(views []AccountView) []column {
	var cols []column
	for _, v := range views {
		if v.Obs == nil {
			continue
		}
		for _, r := range v.Obs.Rows {
			if r.IdentityState == usage.StateBad {
				continue
			}
			if slices.ContainsFunc(cols, func(c column) bool { return c.matches(r) }) {
				continue
			}
			cols = append(cols, column{kind: r.Kind, group: r.Group, model: r.Model, label: Sanitize(r.Label)})
		}
	}
	slices.SortStableFunc(cols, func(a, b column) int {
		return cmp.Or(
			cmp.Compare(columnRank(a.kind), columnRank(b.kind)),
			cmp.Compare(a.model, b.model),
			cmp.Compare(a.kind, b.kind),
			cmp.Compare(a.group, b.group),
		)
	})
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

func (c cell) width() int { return pctWidth + 1 + Cells(c.reset) }

// cellFor is the compact counterpart of LimitRow, and keeps its states apart
// with distinct tokens rather than a legend: a valid future reset, a reset
// legitimately absent (a 5h window nobody has opened), a reset that failed
// to parse, a window that has since ended, and a percent that is not a
// number. The percent's colour precedence is severity's, shared with the
// bar; the reset is dim except when it is the thing that drifted.
func (p Palette) cellFor(r usage.Row, now int64, stale bool) cell {
	c := cell{
		pct:        fmt.Sprintf("%d%%", r.Percent),
		pctColor:   p.severity(r, now, stale),
		reset:      compactReset(r.ResetAt, now),
		resetColor: p.Dim,
	}
	switch {
	case r.ResetState == usage.StateBad:
		c.reset, c.resetColor = "reset?", p.Red
	case r.ResetAt == 0:
		c.reset = "—"
	}
	switch {
	case r.PercentState == usage.StateBad:
		c.pct = "?%"
	case r.RolledOver(now):
		c.pct, c.reset = "?%", "rolled"
	}
	return c
}

// compactReset is ResetPhrase's duration alone, in one token: 4d20h, 2h04m,
// 35m. Only called with a future instant — a past one is a rolled-over
// window, and an absent one has no duration.
func compactReset(resetAt, now int64) string {
	rem := resetAt - now
	d, h, m := rem/86400, rem%86400/3600, rem%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// caption is every clause the block would have said about this account,
// joined on one line: the health warning, the reason there are no figures,
// staleness and provenance, and drift. Never one winner — a logged-out
// account holding a fresh cache is both of those things, and a row that
// picked one would be the axis collapse this model exists to prevent.
func (p Palette) caption(v AccountView, now int64, drift bool) string {
	var parts []string
	if t := healthText(v); t != "" {
		parts = append(parts, p.Red+t+p.Rst)
	}
	switch {
	case v.Obs == nil && v.Health == HealthOK:
		parts = append(parts, p.Dim+"usage unknown — "+p.attemptReason(v, now)+p.Rst)
	case v.Obs != nil && len(v.Obs.Rows) == 0:
		parts = append(parts, p.Dim+"no limits reported"+p.Rst)
	}
	prov, stale := p.provenance(v, now)
	if stale {
		parts = append(parts, p.Yel+"stale"+p.Rst)
	}
	for _, s := range prov {
		parts = append(parts, p.Dim+s+p.Rst)
	}
	if drift {
		parts = append(parts, p.Red+"⚠ drift — run headroom check"+p.Rst)
	}
	return strings.Join(parts, p.Dim+" · "+p.Rst)
}

// compactRow is one account's row before styling: its cells in column order
// (nil when the account has no rows at all — the caption then starts where
// the cells would), and whether anything about it drifted.
type compactRow struct {
	cells []cell
	drift bool
}

func (p Palette) compactBoard(views []AccountView, now int64, width int) Board {
	cols := columns(views)
	rows := make([]compactRow, len(views))
	for i, v := range views {
		if v.Obs == nil || len(v.Obs.Rows) == 0 {
			continue
		}
		stale := !v.Fresh(now)
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
				row.cells[j] = p.cellFor(matched[0], now, stale)
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

	// Column widths: the heading or the widest cell, whichever is wider;
	// the reset slot is the column's widest token so every clock starts in
	// the same place under the same heading.
	for j := range cols {
		for _, row := range rows {
			if row.cells != nil {
				cols[j].resetWidth = max(cols[j].resetWidth, Cells(row.cells[j].reset))
			}
		}
		cols[j].width = max(Cells(cols[j].label), pctWidth+1+cols[j].resetWidth)
	}
	nameW := nameWidth(views, cols, width)

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

// nameWidth sizes the name column: the longest label, capped, and squeezed
// toward the floor when the terminal cannot otherwise leave the caption its
// reserve. The caption is the clip casualty on a narrow screen; giving the
// name column surplus width first would spend the caption's room on padding.
func nameWidth(views []AccountView, cols []column, width int) int {
	longest := 0
	for _, v := range views {
		longest = max(longest, Cells(Sanitize(v.Label)))
	}
	w := min(longest, nameCap)
	if width <= 0 {
		return max(w, 1)
	}
	fixed := selectionPrefix + 1 + marksWidth
	for _, c := range cols {
		fixed += len(colGap) + c.width
	}
	if avail := width - fixed - captionReserve; avail < w {
		w = max(avail, nameFloor)
	}
	return max(w, 1)
}

// compactLine draws one row: name, marks, cells or caption. The name goes
// red on a health problem so the row's worst news survives even when the
// caption that spells it out is clipped off a narrow terminal — green
// figures beside an unusable account is the reading a clip must not produce.
func (p Palette) compactLine(v AccountView, row compactRow, cols []column, nameW int, now int64) string {
	nameColor := p.Bold
	if healthText(v) != "" {
		nameColor = p.Red
	}
	var b strings.Builder
	b.WriteString(nameColor + PadCell(Sanitize(v.Label), nameW) + p.Rst + " ")
	if v.Current {
		b.WriteString(p.Bold + "●" + p.Rst)
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
