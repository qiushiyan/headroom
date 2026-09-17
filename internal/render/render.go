// Package render draws account blocks: a header line plus limit bars or a
// one-line status. Output format matches the reference claude-usage script.
package render

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiushiyan/headroom/internal/accountstate"
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

func (p Palette) HeaderLine(v accountstate.Facts) string {
	label := v.Label
	if v.DirMismatch != "" {
		// /login picked the wrong account in that dir's session — surface it.
		label = fmt.Sprintf("%s %s(dir says %s!)%s", v.Label, p.Red, v.DirMismatch, p.Rst)
	}
	mark := ""
	if v.Current {
		mark = "  " + p.Bold + "← current" + p.Rst
	}
	if v.Plan != "" {
		return fmt.Sprintf("%s%s%s %s(%s · %s)%s%s", p.Bold, label, p.Rst, p.Dim, v.Plan, v.Launcher, p.Rst, mark)
	}
	return fmt.Sprintf("%s%s%s  %s%s%s%s", p.Bold, label, p.Rst, p.Dim, v.Launcher, p.Rst, mark)
}

// HealthLine states what is wrong with the account itself, or "" when nothing
// is. It is rendered independently of any figures: an account needing /login
// can still hold a recent cache, and showing those bars without the warning
// would invite the user to pick an account they cannot use.
func (p Palette) HealthLine(v accountstate.Facts) string {
	if t := healthText(v); t != "" {
		return "  " + p.Red + t + p.Rst
	}
	return ""
}

// healthText is the health clause's words, unstyled and unindented: the
// block wraps them in a red line of their own, the compact row folds them
// into its caption. One spelling, so the two presentations cannot drift.
func healthText(v accountstate.Facts) string {
	switch v.Health {
	case accountstate.HealthNoLogin:
		return fmt.Sprintf("not logged in — run %s and /login", v.Launcher)
	case accountstate.HealthReloginRequired:
		return fmt.Sprintf("login expired — run %s and /login", v.Launcher)
	case accountstate.HealthBadBlob:
		return "credential unreadable — format changed? run headroom check"
	case accountstate.HealthUnknown:
		return "login state unknown — run headroom check"
	default:
		return ""
	}
}

// StatusLine is what an account with no figures at all shows. When health is
// the problem, HealthLine has already said so and there is nothing to add.
func (p Palette) StatusLine(v accountstate.Facts, now int64) string {
	if v.Health != accountstate.HealthOK {
		return p.HealthLine(v)
	}
	return "  " + p.Dim + "usage unknown — " + p.attemptReason(v, now) + p.Rst
}

// attemptReason explains, in the user's terms, why the newest refresh did not
// produce numbers. None of these are statements about the account.
func (p Palette) attemptReason(v accountstate.Facts, now int64) string {
	reason := p.requestReason(v, now)
	if v.Attempt.StoreError != "" && v.Attempt.State != accountstate.AttemptStateUnavailable {
		problem := "headroom's own state file unavailable — run headroom check"
		if v.Attempt.State == accountstate.AttemptOK || (v.Attempt.State == accountstate.AttemptNoLimits && v.Obs != nil) {
			return problem
		}
		return reason + "; " + problem
	}
	return reason
}

func (p Palette) requestReason(v accountstate.Facts, now int64) string {
	switch v.Attempt.State {
	case accountstate.AttemptPending:
		return "fetching…"
	case accountstate.AttemptRefused:
		return "rate limited" + retryPhrase(v.Attempt.NextEligibleAt, now)
	case accountstate.AttemptDeferred:
		return "live check deferred" + retryPhrase(v.Attempt.NextEligibleAt, now)
	case accountstate.AttemptCredentialUnreadable:
		return "credential unreadable — format changed? run headroom check"
	case accountstate.AttemptTokenStale:
		// The access token ages out every ~8 hours and Claude Code refreshes
		// it silently. Saying "expired" here sent the user to /login for a
		// non-problem; the remedy is simply to use the account.
		return fmt.Sprintf("access token stale — any %s session refreshes it", v.Launcher)
	case accountstate.AttemptTransport:
		return "fetch failed (network?)"
	case accountstate.AttemptHTTP:
		return fmt.Sprintf("HTTP %d from usage endpoint", v.Attempt.HTTPCode)
	case accountstate.AttemptUnparseable:
		return "response not parseable — format changed? run headroom check"
	case accountstate.AttemptNoLimits:
		return "no limits reported"
	case accountstate.AttemptStateUnavailable:
		return "headroom's own state file unavailable — run headroom check"
	case accountstate.AttemptIdentityUnknown:
		return "account identity unreadable — .claude.json did not parse"
	default:
		return "not checked"
	}
}

// expected reports that this attempt state says nothing a caption needs to
// carry when the figures are current.
//
// Deferred belongs here and Refused does not, which is the whole reason the
// two are separate states. Deferred is headroom's own politeness — it declined
// to ask because no newer answer is obtainable yet — so over current figures
// there is nothing to report; captioning it would put "live check deferred" on
// a row that is telling the truth right now. Refused is the endpoint saying
// no, which is a fact about the budget worth stating even beside fresh bars.
func expected(s accountstate.AttemptState) bool {
	return s == accountstate.AttemptOK || s == accountstate.AttemptNone || s == accountstate.AttemptDeferred
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
func (p Palette) ProvenanceLine(v accountstate.Facts, now int64) string {
	pr := p.provenance(v, now)
	if pr.empty() {
		return ""
	}
	parts := []string{pr.age}
	if pr.source != "" {
		parts = append(parts, pr.source)
	}
	if pr.attempt != "" {
		parts = append(parts, pr.attempt)
	}
	line := "  " + p.Dim + strings.Join(parts, " · ") + p.Rst
	if pr.stale {
		// Old numbers are context, not an answer — say so where the eye lands.
		line = "  " + p.Yel + "stale" + p.Rst + p.Dim + " · " + strings.Join(parts, " · ") + p.Rst
	}
	return line
}

// provenanceParts is the caption's clauses before layout: the age of the
// figures, their source when it is Claude Code's cache, the newest attempt
// when it went wrong, and whether the figures are stale. Each layout orders
// and styles them its own way; the words are shared so the two cannot drift.
//
// Three independent clauses, never one verdict: how old the figures are,
// where they came from, and how the newest refresh went. All three can be
// true at once — twenty seconds old, from Claude Code's cache, refresh
// refused — and collapsing them is how a failed refresh used to vanish
// behind figures that looked current.
type provenanceParts struct {
	age, source, attempt string
	stale                bool
}

func (pr provenanceParts) empty() bool { return pr.age == "" }

// provenance is empty for the ordinary case — fresh figures headroom fetched
// itself, with nothing to report about the refresh.
func (p Palette) provenance(v accountstate.Facts, now int64) provenanceParts {
	if v.Obs == nil {
		return provenanceParts{}
	}
	fresh := v.Fresh(now)
	if fresh && v.Obs.Source.Ours() && expected(v.Attempt.State) && v.Attempt.StoreError == "" {
		return provenanceParts{}
	}
	pr := provenanceParts{age: "observed " + agePhrase(now-v.Obs.ObservedAt) + " ago", stale: !fresh}
	if v.Obs.Source == accountstate.SourceCache {
		pr.source = "via Claude Code's cache"
	}
	// No limits is already the body of an empty observation; repeating
	// it as a caption would say the same thing twice.
	sayAttempt := v.Attempt.State != accountstate.AttemptOK && v.Attempt.State != accountstate.AttemptNone &&
		!(v.Attempt.State == accountstate.AttemptNoLimits && len(v.Obs.Rows) == 0)
	if sayAttempt || v.Attempt.StoreError != "" {
		pr.attempt = p.attemptReason(v, now)
	}
	return pr
}

// Age is agePhrase for other surfaces: the session picker stamps every row
// with the same relative-time vocabulary the dashboard uses.
func Age(sec int64) string { return agePhrase(sec) }

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
func LabelWidth(views []accountstate.Facts) int {
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

// AccountBlock renders the three axes independently, because they are three
// facts: what is wrong with the account (if anything), what is known about its
// quota, and how the last refresh went. A failed attempt annotates figures
// rather than deleting them, and a health problem is stated whether or not
// figures exist.
func (p Palette) AccountBlock(v accountstate.Facts, now int64, labelWidth int) []string {
	lines := []string{p.HeaderLine(v)}
	if h := p.HealthLine(v); h != "" {
		lines = append(lines, h)
	}
	if v.Obs == nil {
		if v.Health == accountstate.HealthOK {
			lines = append(lines, p.StatusLine(v, now))
		}
		return lines
	}
	if len(v.Obs.Rows) == 0 {
		// A contractual answer, not a failure: this account reports no limit
		// windows at all.
		lines = append(lines, "  "+p.Dim+"no limits reported"+p.Rst)
	}
	for _, r := range v.Obs.Rows {
		lines = append(lines, p.LimitRow(r, now, labelWidth))
	}
	if prov := p.ProvenanceLine(v, now); prov != "" {
		lines = append(lines, prov)
	}
	return lines
}

// LimitRow: `  <label>  [██████░░░░...]  56%  resets Wed 18:00 (in 4.8d)`
// Bars and percentages retain their severity colour regardless of observation
// age; the provenance caption carries staleness separately.
func (p Palette) LimitRow(r usage.Row, now int64, labelWidth int) string {
	color := p.severity(r, now)
	bar := Bar(r.Percent)
	pct := fmt.Sprintf("%3d%%", r.Percent)
	phrase := ResetPhrase(r.ResetAt, now)
	switch {
	case r.PercentState == usage.StateBad:
		// A percent that no longer parses must not read as real headroom.
		bar = strings.Repeat("?", BarWidth)
		pct = "  ?%"
	case r.RolledOver(now):
		// The window ended; whatever is being spent against the new one is
		// unknown. Showing the old number here is the same lie as showing a
		// drifted one — smaller, and in the direction that invites a choice.
		bar = strings.Repeat("·", BarWidth)
		pct = "  ?%"
		phrase = "window rolled over"
	}
	line := fmt.Sprintf("  %-*s %s[%s] %s%s  %s%s%s",
		labelWidth, r.Label, color, bar, pct, p.Rst, p.Dim, phrase, p.Rst)
	if r.Drifted() {
		line += "  " + p.Red + "⚠ drift — run headroom check" + p.Rst
	}
	return line
}

// severity is the one colour rule for bars and percentages in both layouts.
// Observation age does not change severity. A percent that failed to parse
// is red, and a rolled-over window is dim whatever it once read.
func (p Palette) severity(r usage.Row, now int64) string {
	color := p.Grn
	if r.Percent >= 50 {
		color = p.Yel
	}
	if r.Percent >= 80 || r.Severity != "normal" {
		color = p.Red
	}
	switch {
	case r.PercentState == usage.StateBad:
		color = p.Red
	case r.RolledOver(now):
		color = p.Dim
	}
	return color
}

// Clip truncates s to at most width display cells, passing ANSI escape
// sequences through uncut so a truncated line keeps the SGR state changes of
// the full line. Cells, not runes: a CJK session title rendered two cells
// wide would otherwise survive the clip, wrap, and shear framePrinter's
// move-up arithmetic. A wide rune that would straddle the boundary is
// dropped whole.
func Clip(s string, width int) string {
	if width <= 0 {
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
		case cols+runeCells(r) <= width:
			b.WriteRune(r)
			cols += runeCells(r)
		}
	}
	return b.String()
}

func Bar(pct int) string {
	filled := max(min((pct*BarWidth+50)/100, BarWidth), 0)
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
	t := time.Unix(resetAt, 0)
	if rem < 86400 {
		return fmt.Sprintf("resets %s (in %s)", t.Format("15:04"), Remaining(rem))
	}
	return fmt.Sprintf("resets %s (in %s)", t.Format("Mon 15:04"), Remaining(rem))
}

// Remaining is the one spelling of "time until a window resets", in both
// layouts: tenths of an hour under a day (2.1h), tenths of a day from a day
// up (4.8d). One number with one decimal reads faster than a pair of units,
// and the tenth is the precision a choice between accounts actually uses.
// The unit turns over on the rounded value, so 23h58m reads 1.0d rather
// than 24.0h. Only called with a positive remainder.
func Remaining(rem int64) string {
	if h := float64(rem) / 3600; math.Round(h*10)/10 < 24 {
		return fmt.Sprintf("%.1fh", h)
	}
	return fmt.Sprintf("%.1fd", float64(rem)/86400)
}
