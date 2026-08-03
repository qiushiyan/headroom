package render

import "testing"

func TestCells(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"改进会话选择器", 14}, // CJK: two cells each
		{"fix 选择器 bug", 14},
		{"éé", 2},       // narrow despite multibyte encoding
		{"é", 1},       // combining accent adds no width
		{"[████░░]", 8}, // bar glyphs are narrow
	}
	for _, c := range cases {
		if got := Cells(c.in); got != c.want {
			t.Errorf("Cells(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestClipWideRunes(t *testing.T) {
	// A wide rune that would straddle the boundary is dropped whole: clipping
	// 会(2)话(2) to 3 cells yields one rune, 2 cells — never a wrapped line.
	if got := Clip("会话选择", 3); got != "会" {
		t.Errorf("Clip wide to 3 = %q, want 会", got)
	}
	if got := Clip("a会话", 4); got != "a会" {
		t.Errorf("Clip mixed to 4 = %q, want a会", got)
	}
}

func TestTrimAndPadCells(t *testing.T) {
	if got := TrimCells("会话选择", 5); got != "会话" {
		t.Errorf("TrimCells = %q, want 会话", got)
	}
	if got := PadCell("ab", 4); got != "ab  " {
		t.Errorf("PadCell short = %q", got)
	}
	// Trimming to width-1 can stop a cell short of the boundary when the next
	// rune is wide; the pad then fills the field back to exactly width cells.
	if got := PadCell("会话选择", 6); got != "会话… " {
		t.Errorf("PadCell overflow = %q", got)
	}
	if got := Cells(PadCell("会话选", 5)); got != 5 {
		// The ellipsis after trimming to width-1 must still land on width.
		t.Errorf("PadCell wide-boundary width = %d, want 5", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"clean passes untouched", "plain title 标题", "plain title 标题"},
		{"newline becomes space", "a\nb", "a b"},
		{"tab becomes space", "a\tb", "a b"},
		{"crlf collapses to spaces", "a\r\nb", "a  b"},
		{"escape dropped", "a\x1b[31mred", "a[31mred"},
		{"bell dropped", "ding\x07", "ding"},
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
