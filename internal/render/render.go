// Package render draws account blocks: a header line plus limit bars or a
// one-line status. Output format matches the reference claude-usage script.
package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/usage"
)

const BarWidth = 20

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

// AccountBlock renders the header plus limit rows (or the status line).
func (p Palette) AccountBlock(v AccountView, now int64) []string {
	lines := []string{p.HeaderLine(v)}
	if v.Status == StatusRows {
		for _, r := range v.Rows {
			lines = append(lines, p.LimitRow(r, now))
		}
	} else {
		lines = append(lines, p.StatusLine(v))
	}
	return lines
}

// LimitRow: `  <label>  [██████░░░░...]  56%  resets Wed 18:00 (in 4d 20h)`
func (p Palette) LimitRow(r usage.Row, now int64) string {
	color := p.Grn
	if r.Percent >= 50 {
		color = p.Yel
	}
	if r.Percent >= 80 || r.Severity != "normal" {
		color = p.Red
	}
	return fmt.Sprintf("  %-16s %s[%s]%s %3d%%  %s%s%s",
		r.Label, color, Bar(r.Percent), p.Rst, r.Percent, p.Dim, ResetPhrase(r.ResetAt, now), p.Rst)
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
