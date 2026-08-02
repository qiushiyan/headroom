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

// An account carries three facts that vary independently, and the bug this
// model exists to kill was encoding them in one field. All three can be true
// at once: Claude Code says the account is logged in, the newest quota figures
// headroom holds are 22 hours old, and the request it just made was refused.
// Collapsing them means a refused request erases known bars and reads as bad
// news about the account — which is exactly what "token expired" and
// "HTTP 429" were doing.

// Health answers: can Claude Code use this account at all? Only /login fixes
// a bad answer here.
type Health int

const (
	HealthOK              Health = iota // logged in and usable
	HealthNoLogin                       // never logged in on this config dir
	HealthReloginRequired               // refresh token demonstrably expired
	HealthBadBlob                       // credential present but off-contract
	HealthUnknown                       // nothing could establish it either way
)

// Source records where an observation came from, because the two have very
// different freshness guarantees and the user is entitled to know which they
// are looking at.
type Source int

const (
	SourceLive  Source = iota // headroom's own fetch, this run
	SourceCache               // Claude Code's own cache in .claude.json
)

// Observation is quota data plus the provenance that makes it interpretable.
// Rows never travel without ObservedAt: an unlabelled number from an unknown
// time is the thing that made watch's carried-over bars a lie.
type Observation struct {
	Rows       []usage.Row
	ObservedAt int64 // unix seconds
	Source     Source
}

// AttemptState is what happened the last time headroom tried to refresh —
// about the *request*, never about the account.
type AttemptState int

const (
	AttemptNone        AttemptState = iota // no attempt this run
	AttemptPending                         // in flight
	AttemptOK                              // fresh rows landed
	AttemptRefused                         // HTTP 429 — says nothing about the account
	AttemptDeferred                        // not attempted: still inside a quiet period
	AttemptTokenStale                      // access token aged out; a session refreshes it
	AttemptTransport                       // network or timeout
	AttemptHTTP                            // some other non-200
	AttemptUnparseable                     // 200 whose body failed the contract
	AttemptNoLimits                        // 200 reporting no limit windows
)

// Attempt is the outcome of the newest refresh, with the time the account
// becomes eligible again where one applies.
type Attempt struct {
	State          AttemptState
	HTTPCode       int
	NextEligibleAt int64 // unix seconds; 0 = eligible now
}

// FreshWindow bounds how recent an observation must be to count as current
// headroom. It is a display and ranking policy, not a vendor promise — quota
// can move the moment after a fetch. Anything older still renders, labelled
// with its age, because stale context beats a blank.
const FreshWindow = 90 * time.Second

// AccountView is everything needed to draw one account.
type AccountView struct {
	Label       string
	DirMismatch string // dir name when the logged-in email doesn't match it
	Plan        string
	Launcher    string
	Current     bool // bare `x` targets this account
	Health      Health
	Obs         *Observation // nil = nothing known
	Attempt     Attempt
}

// Fresh reports whether the observation is recent enough to describe current
// headroom. The picker must not rank an account on anything else.
func (v AccountView) Fresh(now int64) bool {
	return v.Obs != nil && now-v.Obs.ObservedAt <= int64(FreshWindow/time.Second)
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

// StatusLine is what an account shows when nothing at all is known about its
// quota. Health outranks attempt here: a dead login is the user's problem to
// fix, while a refused request is headroom's problem to wait out.
func (p Palette) StatusLine(v AccountView, now int64) string {
	switch v.Health {
	case HealthNoLogin:
		return fmt.Sprintf("  %snot logged in — run %s and /login%s", p.Dim, v.Launcher, p.Rst)
	case HealthReloginRequired:
		return fmt.Sprintf("  %slogin expired — run %s and /login%s", p.Dim, v.Launcher, p.Rst)
	case HealthBadBlob:
		return "  " + p.Dim + "credential blob unreadable — format changed? run headroom check" + p.Rst
	case HealthUnknown:
		return "  " + p.Dim + "login state unknown — run headroom check" + p.Rst
	}
	return "  " + p.Dim + "usage unknown — " + p.attemptReason(v, now) + p.Rst
}

// attemptReason explains, in the user's terms, why the newest refresh did not
// produce numbers. None of these are statements about the account.
func (p Palette) attemptReason(v AccountView, now int64) string {
	switch v.Attempt.State {
	case AttemptPending:
		return "fetching…"
	case AttemptRefused:
		return "rate limited" + retryPhrase(v.Attempt.NextEligibleAt, now)
	case AttemptDeferred:
		return "live check deferred" + retryPhrase(v.Attempt.NextEligibleAt, now)
	case AttemptTokenStale:
		// The access token ages out every ~8 hours and Claude Code refreshes
		// it silently. Saying "expired" here sent the user to /login for a
		// non-problem; the remedy is simply to use the account.
		return fmt.Sprintf("access token stale — any %s session refreshes it", v.Launcher)
	case AttemptTransport:
		return "fetch failed (network?)"
	case AttemptHTTP:
		return fmt.Sprintf("HTTP %d from usage endpoint", v.Attempt.HTTPCode)
	case AttemptUnparseable:
		return "response not parseable — format changed? run headroom check"
	case AttemptNoLimits:
		return "no limits reported"
	default:
		return "not checked"
	}
}

func retryPhrase(nextAt, now int64) string {
	if nextAt <= now {
		return ""
	}
	rem := nextAt - now
	if rem < 60 {
		return fmt.Sprintf("; next attempt in %ds", rem)
	}
	return fmt.Sprintf("; next attempt in %dm", (rem+59)/60)
}

// ProvenanceLine annotates rows with their age, their source, and whatever
// went wrong refreshing them. It returns "" for the ordinary case — fresh
// numbers straight from the endpoint need no caption.
func (p Palette) ProvenanceLine(v AccountView, now int64) string {
	if v.Obs == nil {
		return ""
	}
	fresh := v.Fresh(now)
	quiet := fresh && v.Obs.Source == SourceLive &&
		(v.Attempt.State == AttemptOK || v.Attempt.State == AttemptNone)
	if quiet {
		return ""
	}
	parts := []string{"observed " + agePhrase(now-v.Obs.ObservedAt) + " ago"}
	if v.Obs.Source == SourceCache {
		parts = append(parts, "via Claude Code's cache")
	}
	if r := p.attemptReason(v, now); v.Attempt.State != AttemptOK && v.Attempt.State != AttemptNone {
		parts = append(parts, r)
	}
	line := "  " + p.Dim + strings.Join(parts, " · ") + p.Rst
	if !fresh {
		// Old numbers are context, not an answer — say so where the eye lands.
		line = "  " + p.Yel + "stale" + p.Rst + p.Dim + " · " + strings.Join(parts, " · ") + p.Rst
	}
	return line
}

func agePhrase(sec int64) string {
	switch {
	case sec < 1:
		return "just now"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	default:
		return fmt.Sprintf("%dd", sec/86400)
	}
}

// LabelWidth is the label column width for a set of views: wide enough for
// the longest label anywhere, so every account's bars align, and never
// narrower than the classic column.
func LabelWidth(views []AccountView) int {
	w := minLabelWidth
	for _, v := range views {
		if v.Obs == nil {
			continue
		}
		for _, r := range v.Obs.Rows {
			if n := utf8.RuneCountInString(r.Label); n > w {
				w = n
			}
		}
	}
	return w
}

// AccountBlock renders the header plus whatever is known: limit rows when an
// observation exists — regardless of how the newest refresh went — otherwise
// a line explaining what is missing. A failed attempt annotates the rows; it
// never deletes them.
func (p Palette) AccountBlock(v AccountView, now int64, labelWidth int) []string {
	lines := []string{p.HeaderLine(v)}
	if v.Obs == nil {
		return append(lines, p.StatusLine(v, now))
	}
	stale := !v.Fresh(now)
	for _, r := range v.Obs.Rows {
		lines = append(lines, p.LimitRow(r, now, labelWidth, stale))
	}
	if prov := p.ProvenanceLine(v, now); prov != "" {
		lines = append(lines, prov)
	}
	return lines
}

// LimitRow: `  <label>  [██████░░░░...]  56%  resets Wed 18:00 (in 4d 20h)`
// A stale row keeps its numbers but loses its severity colour, so an old 12%
// can't be mistaken at a glance for headroom available right now.
func (p Palette) LimitRow(r usage.Row, now int64, labelWidth int, stale bool) string {
	color := p.Grn
	if r.Percent >= 50 {
		color = p.Yel
	}
	if r.Percent >= 80 || r.Severity != "normal" {
		color = p.Red
	}
	if stale {
		color = p.Dim
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
