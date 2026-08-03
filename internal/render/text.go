package render

import (
	"strings"
	"unicode"
)

// Cell-width text handling for vendor-supplied strings. Session titles and
// prompt previews are the first fields headroom renders that a human typed —
// possibly in a double-width script — so column arithmetic must count
// display cells, not runes: framePrinter's move-up cursor math shears the
// moment one rendered line wraps.

// wide covers the ranges terminals render two cells wide: East Asian Wide
// and Fullwidth, plus the emoji planes. A partial table on purpose — exotic
// misses cost one cell of misalignment on one row, which is the right price
// for not importing a width library.
var wide = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1100, Hi: 0x115F, Stride: 1}, // Hangul Jamo
		{Lo: 0x2E80, Hi: 0x303E, Stride: 1}, // CJK radicals, kana punctuation
		{Lo: 0x3041, Hi: 0x33FF, Stride: 1}, // Hiragana … CJK compatibility
		{Lo: 0x3400, Hi: 0x4DBF, Stride: 1}, // CJK extension A
		{Lo: 0x4E00, Hi: 0x9FFF, Stride: 1}, // CJK unified
		{Lo: 0xA000, Hi: 0xA4CF, Stride: 1}, // Yi
		{Lo: 0xAC00, Hi: 0xD7A3, Stride: 1}, // Hangul syllables
		{Lo: 0xF900, Hi: 0xFAFF, Stride: 1}, // CJK compatibility ideographs
		{Lo: 0xFE30, Hi: 0xFE4F, Stride: 1}, // CJK compatibility forms
		{Lo: 0xFF00, Hi: 0xFF60, Stride: 1}, // fullwidth forms
		{Lo: 0xFFE0, Hi: 0xFFE6, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1F300, Hi: 0x1FAFF, Stride: 1}, // emoji
		{Lo: 0x20000, Hi: 0x3FFFD, Stride: 1}, // CJK extensions B+
	},
}

// runeCells is the display width of one rune: 0 for combining marks and
// format controls, 2 for wide glyphs, 1 otherwise.
func runeCells(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case unicode.Is(wide, r):
		return 2
	default:
		return 1
	}
}

// Cells is the display width of a plain string (no ANSI sequences).
func Cells(s string) int {
	n := 0
	for _, r := range s {
		n += runeCells(r)
	}
	return n
}

// PadCell left-aligns s in a field of the given cell width, truncating with
// an ellipsis when it doesn't fit. Rune-count padding misaligns every column
// to the right of a CJK label; this is the cell-aware replacement.
func PadCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if Cells(s) > width {
		s = TrimCells(s, width-1) + "…"
	}
	return s + strings.Repeat(" ", width-Cells(s))
}

// TrimCells truncates a plain string to at most the given cell width. A wide
// rune that would straddle the boundary is dropped whole.
func TrimCells(s string, width int) string {
	var b strings.Builder
	cols := 0
	for _, r := range s {
		w := runeCells(r)
		if cols+w > width {
			break
		}
		b.WriteRune(r)
		cols += w
	}
	return b.String()
}

// Sanitize makes vendor text safe to place inside one rendered line: session
// titles, prompt previews and branch names come from files another program
// writes, and a control byte in them could inject cursor movement or split a
// line and shear the frame arithmetic. Newlines and tabs become spaces; every
// other control rune is dropped.
func Sanitize(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// dropped: ESC and friends have no honest visible form
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
