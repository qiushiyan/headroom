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

	// HealthUnprobed is a statement about the surface, not the account: this
	// run chose not to ask (the limits surface skips the ~170ms-per-account
	// auth probe). It is distinct from HealthUnknown — "we asked and could
	// not establish it" — because a read that skipped the question must not
	// let its silence pass for an answer.
	HealthUnprobed
)

// Source records where an observation came from, because the two have very
// different freshness guarantees and the user is entitled to know which they
// are looking at.
type Source int

const (
	SourceLive  Source = iota // headroom's own fetch, in this process
	SourceStore               // headroom's own fetch, replayed from the state file
	SourceCache               // Claude Code's own cache in .claude.json
)

// Ours reports that headroom fetched this observation itself, whether in this
// process or an earlier one. The distinction between the two matters to a
// machine consumer — it is the difference between "this run asked" and "a
// previous run asked" — but not to the caption: both are headroom's own
// reading of the endpoint, at the time the observation carries.
func (s Source) Ours() bool { return s == SourceLive || s == SourceStore }

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
	AttemptNone       AttemptState = iota // no attempt this run
	AttemptPending                        // in flight
	AttemptOK                             // fresh rows landed
	AttemptRefused                        // HTTP 429 — says nothing about the account
	AttemptDeferred                       // not attempted: still inside a quiet period
	AttemptTokenStale                     // access token aged out; a session refreshes it
	// AttemptCredentialUnreadable: headroom can't read a credential to spend.
	// That blocks *our* request; it is not a statement about the account, so
	// it lives here and not on the health axis.
	AttemptCredentialUnreadable
	AttemptTransport   // network or timeout
	AttemptHTTP        // some other non-200
	AttemptUnparseable // 200 whose body failed the contract
	AttemptNoLimits    // 200 reporting no limit windows
	// AttemptStateUnavailable: headroom's own state file could not be read or
	// claimed against, so no request was authorized. Like every value here it
	// is about the request; unlike the others the fault is headroom's own.
	AttemptStateUnavailable
	// AttemptIdentityUnknown: this account's .claude.json could not be parsed,
	// so which per-account budget it shares is unknown. Asking anyway would
	// spend against a bucket that might not be its own.
	AttemptIdentityUnknown
)

// Attempt is the outcome of the newest refresh, with the time the account
// becomes eligible again where one applies.
type Attempt struct {
	State          AttemptState
	HTTPCode       int
	NextEligibleAt int64 // unix seconds; 0 = eligible now
}

// FreshWindow bounds how recent an observation must be to count as current
// headroom. It is a display policy, not a vendor promise — quota can move the
// moment after a fetch. Anything older still renders, labelled with its age,
// because stale context beats a blank.
//
// It is the request spacing rather than a number of its own: inside that
// window no newer answer is obtainable, so nagging that figures are old would
// be nagging about something nobody can act on. Two independent 90s constants
// happened to be equal, and the day one moved the other would have started
// marking every second run stale.
const FreshWindow = usage.RequestSpacing

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
// headroom.
func (v AccountView) Fresh(now int64) bool {
	return v.Obs != nil && now-v.Obs.ObservedAt <= int64(FreshWindow/time.Second)
}

// Actionable is the question the picker actually asks: are these figures
// grounds for choosing this account right now? Freshness alone is not —
// a logged-out account can hold a cache minutes old, and offering it as a
// live choice on that basis is the mistake this model exists to prevent.
//
// Nor is a fresh observation enough on its own. A row whose own reset instant
// has passed describes a window that has since ended: the percent is a fact
// about the past, and a *low* one reads as headroom that may not exist.
func (v AccountView) Actionable(now int64) bool {
	return v.Health == HealthOK && v.Fresh(now) && !v.RolledOver(now)
}

// RolledOver reports that some row has outlived the window it describes.
func (v AccountView) RolledOver(now int64) bool {
	if v.Obs == nil {
		return false
	}
	for _, r := range v.Obs.Rows {
		if r.RolledOver(now) {
			return true
		}
	}
	return false
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

// HealthLine states what is wrong with the account itself, or "" when nothing
// is. It is rendered independently of any figures: an account needing /login
// can still hold a recent cache, and showing those bars without the warning
// would invite the user to pick an account they cannot use.
func (p Palette) HealthLine(v AccountView) string {
	switch v.Health {
	case HealthNoLogin:
		return fmt.Sprintf("  %snot logged in — run %s and /login%s", p.Red, v.Launcher, p.Rst)
	case HealthReloginRequired:
		return fmt.Sprintf("  %slogin expired — run %s and /login%s", p.Red, v.Launcher, p.Rst)
	case HealthBadBlob:
		return "  " + p.Red + "credential unreadable — format changed? run headroom check" + p.Rst
	case HealthUnknown:
		return "  " + p.Red + "login state unknown — run headroom check" + p.Rst
	default:
		return ""
	}
}

// StatusLine is what an account with no figures at all shows. When health is
// the problem, HealthLine has already said so and there is nothing to add.
func (p Palette) StatusLine(v AccountView, now int64) string {
	if v.Health != HealthOK {
		return p.HealthLine(v)
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
	case AttemptCredentialUnreadable:
		return "credential unreadable — format changed? run headroom check"
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
	case AttemptStateUnavailable:
		return "headroom's own state file unavailable — run headroom check"
	case AttemptIdentityUnknown:
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
func expected(s AttemptState) bool {
	return s == AttemptOK || s == AttemptNone || s == AttemptDeferred
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
	// Three independent clauses, never one verdict: how old the figures are,
	// where they came from, and how the newest refresh went. All three can be
	// true at once — twenty seconds old, from Claude Code's cache, refresh
	// refused — and collapsing them is how a failed refresh used to vanish
	// behind figures that looked current.
	fresh := v.Fresh(now)
	if fresh && v.Obs.Source.Ours() && expected(v.Attempt.State) {
		return ""
	}
	parts := []string{"observed " + agePhrase(now-v.Obs.ObservedAt) + " ago"}
	if v.Obs.Source == SourceCache {
		parts = append(parts, "via Claude Code's cache")
	}
	// AttemptNoLimits is already the body of an empty observation; repeating
	// it as a caption would say the same thing twice.
	sayAttempt := v.Attempt.State != AttemptOK && v.Attempt.State != AttemptNone &&
		!(v.Attempt.State == AttemptNoLimits && len(v.Obs.Rows) == 0)
	if sayAttempt {
		parts = append(parts, p.attemptReason(v, now))
	}
	line := "  " + p.Dim + strings.Join(parts, " · ") + p.Rst
	if !fresh {
		// Old numbers are context, not an answer — say so where the eye lands.
		line = "  " + p.Yel + "stale" + p.Rst + p.Dim + " · " + strings.Join(parts, " · ") + p.Rst
	}
	return line
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

// AccountBlock renders the three axes independently, because they are three
// facts: what is wrong with the account (if anything), what is known about its
// quota, and how the last refresh went. A failed attempt annotates figures
// rather than deleting them, and a health problem is stated whether or not
// figures exist.
func (p Palette) AccountBlock(v AccountView, now int64, labelWidth int) []string {
	lines := []string{p.HeaderLine(v)}
	if h := p.HealthLine(v); h != "" {
		lines = append(lines, h)
	}
	if v.Obs == nil {
		if v.Health == HealthOK {
			lines = append(lines, p.StatusLine(v, now))
		}
		return lines
	}
	if len(v.Obs.Rows) == 0 {
		// A contractual answer, not a failure: this account reports no limit
		// windows at all.
		lines = append(lines, "  "+p.Dim+"no limits reported"+p.Rst)
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
	phrase := ResetPhrase(r.ResetAt, now)
	switch {
	case r.PercentState == usage.StateBad:
		// A percent that no longer parses must not read as real headroom.
		color = p.Red
		bar = strings.Repeat("?", BarWidth)
		pct = "  ?%"
	case r.RolledOver(now):
		// The window ended; whatever is being spent against the new one is
		// unknown. Showing the old number here is the same lie as showing a
		// drifted one — smaller, and in the direction that invites a choice.
		color = p.Dim
		bar = strings.Repeat("·", BarWidth)
		pct = "  ?%"
		phrase = "window rolled over"
	}
	line := fmt.Sprintf("  %-*s %s[%s]%s %s  %s%s%s",
		labelWidth, r.Label, color, bar, p.Rst, pct, p.Dim, phrase, p.Rst)
	if r.Drifted() {
		line += "  " + p.Red + "⚠ drift — run headroom check" + p.Rst
	}
	return line
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
