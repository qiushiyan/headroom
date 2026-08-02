// Package render draws account blocks: a header line plus limit bars or a
// one-line status. Output format matches the reference claude-usage script.
package render

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiushiyan/headroom/internal/usage"
)

const (
	BarWidth = 20

	// minLabelWidth keeps the classic column for the usual short labels;
	// LabelWidth widens it only when some label needs more.
	minLabelWidth = 16
)

type Palette struct {
	Bold, Dim, Red, Yel, Grn, Rev, Rst string
}

func NewPalette(color bool) Palette {
	if !color {
		return Palette{}
	}
	return Palette{
		Bold: "\x1b[1m", Dim: "\x1b[2m", Red: "\x1b[31m",
		Yel: "\x1b[33m", Grn: "\x1b[32m", Rev: "\x1b[7m", Rst: "\x1b[0m",
	}
}

type Status int

const (
	StatusRows        Status = iota // render Rows
	StatusPending                   // fetch still in flight (select picker)
	StatusNoLogin                   // no credential blob anywhere
	StatusBadBlob                   // blob present but fails the contract
	StatusExpired                   // token expired; a Claude session refreshes it
	StatusFetchFailed               // transport error
	StatusHTTPError                 // non-200 from the endpoint
	StatusUnparseable               // response fails the contract
	StatusNoLimits                  // 200 but zero limit rows
)

// AccountView is everything needed to draw one account.
type AccountView struct {
	Label       string
	DirMismatch string // dir name when the logged-in email doesn't match it
	Plan        string
	Launcher    string
	Current     bool // bare `x` targets this account
	Status      Status
	HTTPCode    int
	Rows        []usage.Row
}

func (p Palette) HeaderLine(v AccountView) string {
	label := v.Label
	if v.DirMismatch != "" {
		// /login picked the wrong account in that dir's session — surface it.
		label = fmt.Sprintf("%s %s(dir says %s!)%s", v.Label, p.Red, v.DirMismatch, p.Rst)
	}
	mark := ""
	if v.Current {
		mark = "  " + p.Bold + "← x" + p.Rst
	}
	if v.Plan != "" {
		return fmt.Sprintf("%s%s%s %s(%s · %s)%s%s", p.Bold, label, p.Rst, p.Dim, v.Plan, v.Launcher, p.Rst, mark)
	}
	return fmt.Sprintf("%s%s%s  %s%s%s%s", p.Bold, label, p.Rst, p.Dim, v.Launcher, p.Rst, mark)
}

func (p Palette) StatusLine(v AccountView) string {
	switch v.Status {
	case StatusPending:
		return "  " + p.Dim + "fetching…" + p.Rst
	case StatusNoLogin:
		return fmt.Sprintf("  %snot logged in — run %s and /login%s", p.Dim, v.Launcher, p.Rst)
	case StatusBadBlob:
		return "  " + p.Dim + "credential blob unreadable — format changed? run headroom check" + p.Rst
	case StatusExpired:
		return fmt.Sprintf("  %stoken expired — run %s to refresh it%s", p.Dim, v.Launcher, p.Rst)
	case StatusFetchFailed:
		return "  " + p.Dim + "fetch failed (network?)" + p.Rst
	case StatusHTTPError:
		return fmt.Sprintf("  %sHTTP %d from usage endpoint%s", p.Dim, v.HTTPCode, p.Rst)
	case StatusUnparseable:
		return "  " + p.Dim + "response not parseable — format changed? run headroom check" + p.Rst
	case StatusNoLimits:
		return "  " + p.Dim + "no limits reported" + p.Rst
	default:
		return ""
	}
}

// LabelWidth is the label column width for a set of views: wide enough for
// the longest label anywhere, so every account's bars align, and never
// narrower than the classic column.
func LabelWidth(views []AccountView) int {
	w := minLabelWidth
	for _, v := range views {
		for _, r := range v.Rows {
			if n := utf8.RuneCountInString(r.Label); n > w {
				w = n
			}
		}
	}
	return w
}

// AccountBlock renders the header plus limit rows (or the status line).
func (p Palette) AccountBlock(v AccountView, now int64, labelWidth int) []string {
	lines := []string{p.HeaderLine(v)}
	if v.Status == StatusRows {
		for _, r := range v.Rows {
			lines = append(lines, p.LimitRow(r, now, labelWidth))
		}
	} else {
		lines = append(lines, p.StatusLine(v))
	}
	return lines
}

// LimitRow: `  <label>  [██████░░░░...]  56%  resets Wed 18:00 (in 4d 20h)`
func (p Palette) LimitRow(r usage.Row, now int64, labelWidth int) string {
	color := p.Grn
	if r.Percent >= 50 {
		color = p.Yel
	}
	if r.Percent >= 80 || r.Severity != "normal" {
		color = p.Red
	}
	bar := Bar(r.Percent)
	pct := fmt.Sprintf("%3d%%", r.Percent)
	if r.PercentState == usage.StateBad {
		// A percent that no longer parses must not read as real headroom.
		color = p.Red
		bar = strings.Repeat("?", BarWidth)
		pct = "  ?%"
	}
	line := fmt.Sprintf("  %-*s %s[%s]%s %s  %s%s%s",
		labelWidth, r.Label, color, bar, p.Rst, pct, p.Dim, ResetPhrase(r.ResetAt, now), p.Rst)
	if r.Drifted() {
		line += "  " + p.Red + "⚠ drift — run headroom check" + p.Rst
	}
	return line
}

// Clip truncates s to at most width printable columns, passing ANSI escape
// sequences through uncut so a truncated line keeps the SGR state changes of
// the full line. Every printable rune counts one column — true for all the
// glyphs this program emits.
func Clip(s string, width int) string {
	if width <= 0 || utf8.RuneCountInString(s) <= width {
		return s
	}
	var b strings.Builder
	cols, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			b.WriteRune(r)
			// A CSI sequence (ESC [ params letter) ends at its final byte;
			// '[' right after ESC is the sequence introducer, not a final.
			if r >= '@' && r <= '~' && r != '[' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
			b.WriteRune(r)
		case cols < width:
			b.WriteRune(r)
			cols++
		}
	}
	return b.String()
}

func Bar(pct int) string {
	filled := (pct*BarWidth + 50) / 100
	if filled > BarWidth {
		filled = BarWidth
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", BarWidth-filled)
}

func ResetPhrase(resetAt, now int64) string {
	if resetAt == 0 {
		return "resets ?"
	}
	rem := resetAt - now
	if rem <= 0 {
		return "resetting…"
	}
	d, h, m := rem/86400, rem%86400/3600, rem%3600/60
	var in string
	switch {
	case d > 0:
		in = fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		in = fmt.Sprintf("%dh %dm", h, m)
	default:
		in = fmt.Sprintf("%dm", m)
	}
	t := time.Unix(resetAt, 0)
	if rem < 86400 {
		return fmt.Sprintf("resets %s (in %s)", t.Format("15:04"), in)
	}
	return fmt.Sprintf("resets %s (in %s)", t.Format("Mon 15:04"), in)
}
