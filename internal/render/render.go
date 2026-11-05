// Package render draws what headroom shows a person: the accounts board in
// both of its layouts (Board — the block per account and the compact row),
// the session picker's rows and preview, and the cell-width text handling
// both rest on. It decides no facts: it spells the ones accountstate and
// sessions hand it, and the callers own the terminal — clipping, framing and
// the in-place redraw.
package render

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

const (
	barWidth = 20

	// minLabelWidth keeps the classic column for the usual short labels;
	// labelWidth widens it only when some label needs more.
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

// headerLine is the block's first line: the account, its plan and launcher,
// what it is reserved for, and "← next" on the account a bare launch would
// take.
func (p Palette) headerLine(v accountstate.Facts) string {
	// Both names come from files another program writes (the dir, the
	// vendor's own record of the login), so they are sanitized like the
	// compact row's.
	label := Sanitize(v.Label)
	if v.DirMismatch != "" {
		// /login picked the wrong account in that dir's session — surface it.
		label = fmt.Sprintf("%s %s(dir says %s!)%s", label, p.Red, Sanitize(v.DirMismatch), p.Rst)
	}
	mark := ""
	if v.Reserved != "" {
		mark = "  " + p.Dim + Sanitize(v.Reserved) + p.Rst
	}
	if v.Next {
		mark += "  " + p.Bold + "← next" + p.Rst
	}
	if v.Plan != "" {
		return fmt.Sprintf("%s%s%s %s(%s · %s)%s%s", p.Bold, label, p.Rst, p.Dim, v.Plan, v.Launcher, p.Rst, mark)
	}
	return fmt.Sprintf("%s%s%s  %s%s%s%s", p.Bold, label, p.Rst, p.Dim, v.Launcher, p.Rst, mark)
}

// healthText is the health clause's words, unstyled and unindented: the
// block wraps them in a red line of their own, the compact row folds them
// into its caption. One spelling, so the two presentations cannot drift.
func healthText(v accountstate.Facts) string {
	switch v.Health {
	case accountstate.HealthNoLogin:
		return "not logged in — run " + loginHint(v)
	case accountstate.HealthReloginRequired:
		return "login expired — run " + loginHint(v)
	case accountstate.HealthBadBlob:
		return "credential unreadable — format changed? run headroom check"
	case accountstate.HealthUnknown:
		if v.AuthMode != "" {
			// Not a subscription login: there is no window to read, and
			// nothing is wrong with the account.
			return fmt.Sprintf("auth mode %s — no subscription usage to read", Sanitize(v.AuthMode))
		}
		return "login state unknown — run headroom check"
	default:
		return ""
	}
}

// loginHint is how to log this account in. Claude Code logs in from inside a
// session (/login); Codex has no such command, so its hint is one whole
// command line and never says /login.
func loginHint(v accountstate.Facts) string {
	if v.LoginCommand != "" {
		return v.LoginCommand
	}
	return v.Launcher + " and /login"
}

// allowanceText is the account-level clause: positive blocking evidence, in
// red because it decides the choice, or "" when the vendor has said nothing
// that blocks.
func allowanceText(v accountstate.Facts) string {
	if !v.Blocked() {
		return ""
	}
	return "blocked — " + blockReason(v.Obs.Allowance.Reason)
}

// blockReason spells the allowance's reason for a person. The parser hands
// over the vendor's own reason string when it sent one, and otherwise the name
// of the field that said so; unknown vendor vocabulary is shown as it came.
func blockReason(reason string) string {
	switch reason {
	case "":
		return "the vendor refuses work on this account"
	case "spend_control.reached":
		return "spend limit reached"
	case "rate_limit.limit_reached":
		return "usage limit reached"
	case "rate_limit.allowed is false":
		return "the vendor refuses work on this account"
	default:
		return strings.ReplaceAll(Sanitize(reason), "_", " ")
	}
}

// featureText names the limits blocked on their own. They never make the
// account unavailable, so they are a caption and nothing more.
func featureText(v accountstate.Facts) string {
	if v.Obs == nil || len(v.Obs.Allowance.BlockedFeatures) == 0 {
		return ""
	}
	names := make([]string, len(v.Obs.Allowance.BlockedFeatures))
	for i, f := range v.Obs.Allowance.BlockedFeatures {
		names[i] = Sanitize(f)
	}
	return strings.Join(names, ", ") + " blocked"
}

// allowanceDrifted reports a blocking field that no longer parses.
func allowanceDrifted(v accountstate.Facts) bool {
	return v.Obs != nil && v.Obs.Allowance.State == usage.AllowanceBad
}

// unknownText is what a healthy account with no figures at all says, in both
// layouts: that its usage is unknown, and why the newest refresh did not
// produce numbers. An unhealthy one says nothing more — its health clause
// already names the problem.
func unknownText(v accountstate.Facts, now int64) string {
	return "usage unknown — " + attemptReason(v, now)
}

// attemptReason explains, in the user's terms, why the newest refresh did not
// produce numbers. None of these are statements about the account.
func attemptReason(v accountstate.Facts, now int64) string {
	reason := requestReason(v, now)
	if v.Attempt.StoreError != "" && v.Attempt.State != accountstate.AttemptStateUnavailable {
		problem := "headroom's own state file unavailable — run headroom check"
		if v.Attempt.State == accountstate.AttemptOK || (v.Attempt.State == accountstate.AttemptNoLimits && v.Obs != nil) {
			return problem
		}
		return reason + "; " + problem
	}
	return reason
}

func requestReason(v accountstate.Facts, now int64) string {
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
		if v.Vendor == config.Codex && v.Attempt.HTTPCode == 401 {
			// Codex itself recovers from a 401 by reloading and refreshing, so
			// a rejected token is not evidence that a person must log in.
			return fmt.Sprintf("access token rejected — any %s session refreshes it", v.Launcher)
		}
		return fmt.Sprintf("HTTP %d from usage endpoint", v.Attempt.HTTPCode)
	case accountstate.AttemptUnparseable:
		return "response not parseable — format changed? run headroom check"
	case accountstate.AttemptNoLimits:
		return "no limits reported"
	case accountstate.AttemptStateUnavailable:
		return "headroom's own state file unavailable — run headroom check"
	case accountstate.AttemptIdentityUnknown:
		if v.Vendor == config.Codex {
			return "account identity unreadable — auth.json names no account and user"
		}
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

// A clause is one thing a layout says about an account beside its figures,
// styled. Both layouts say the same clauses, decided here once; the compact
// row joins them in their kinds' order, which is clip order, and the block
// places them on its lines by narrative. Two layouts deciding them apart is
// how a row comes to collapse what the block keeps apart.
//
// Every applicable clause, never one verdict: figures twenty seconds old,
// from Claude Code's cache, whose refresh was refused, are all three at once,
// and collapsing them is how a failed refresh used to vanish behind figures
// that looked current.
type clauseKind uint8

const (
	// What changes what the user does comes first: a narrow terminal cuts
	// the compact caption from the right.
	clauseHealth    clauseKind = iota // what is wrong with the account itself
	clauseAllowance                   // the vendor refuses work on it, whatever the figures say
	clauseMismatch                    // the dir holds another login
	clauseEmpty                       // why there are no figures, or that the vendor reports none
	clauseDrift                       // something no longer parses
	clauseFeature                     // limits blocked on their own; the account stays usable
	clauseReserved                    // what the account is kept for
	clauseStale                       // the figures are too old to choose on
	clauseAttempt                     // how the newest refresh went
	// Then what explains.
	clauseAge    // how old the figures are
	clauseSource // where they came from, when not headroom's own fetch
	clauseLoad   // the sessions and launches on the account
	clauseKinds
)

type clauses [clauseKinds]string

// clauses decides every clause for one account. rowDrift is drift in its
// rows: the compact row says it in its one drift clause beside the
// allowance's, while the block marks each drifted row where it stands.
func (p Palette) clauses(v accountstate.Facts, now int64, rowDrift bool) clauses {
	var c clauses
	if t := healthText(v); t != "" {
		c[clauseHealth] = p.Red + t + p.Rst
	}
	if t := allowanceText(v); t != "" {
		c[clauseAllowance] = p.Red + t + p.Rst
	}
	if v.DirMismatch != "" {
		c[clauseMismatch] = p.Red + "dir says " + Sanitize(v.DirMismatch) + p.Rst
	}
	switch {
	case v.Obs == nil && v.Health == accountstate.HealthOK:
		// An unhealthy account says nothing more: its health names the problem.
		c[clauseEmpty] = p.Dim + unknownText(v, now) + p.Rst
	case v.Obs != nil && len(v.Obs.Rows) == 0:
		// A contractual answer, not a failure.
		c[clauseEmpty] = p.Dim + "no limits reported" + p.Rst
	}
	if rowDrift || allowanceDrifted(v) {
		c[clauseDrift] = p.Red + "⚠ drift — run headroom check" + p.Rst
	}
	if t := featureText(v); t != "" {
		c[clauseFeature] = p.Yel + t + p.Rst
	}
	if v.Reserved != "" {
		c[clauseReserved] = p.Dim + Sanitize(v.Reserved) + p.Rst
	}
	if t := loadText(v.Load); t != "" {
		c[clauseLoad] = p.Dim + t + p.Rst
	}
	if v.Obs == nil {
		return c
	}
	fresh := v.Fresh(now)
	if fresh && v.Obs.Source.Ours() && expected(v.Attempt.State) && v.Attempt.StoreError == "" {
		// The ordinary case — fresh figures headroom fetched itself, nothing
		// to report about the refresh — says nothing about where they came from.
		return c
	}
	if !fresh {
		// Old numbers are context, not an answer — say so where the eye lands.
		c[clauseStale] = p.Yel + "stale" + p.Rst
	}
	c[clauseAge] = p.Dim + "observed " + Age(now-v.Obs.ObservedAt) + " ago" + p.Rst
	if v.Obs.Source == accountstate.SourceCache {
		c[clauseSource] = p.Dim + "via Claude Code's cache" + p.Rst
	}
	// No limits is already the body of an empty observation; repeating it as
	// a caption would say the same thing twice.
	sayAttempt := v.Attempt.State != accountstate.AttemptOK && v.Attempt.State != accountstate.AttemptNone &&
		!(v.Attempt.State == accountstate.AttemptNoLimits && len(v.Obs.Rows) == 0)
	if sayAttempt || v.Attempt.StoreError != "" {
		c[clauseAttempt] = p.Dim + attemptReason(v, now) + p.Rst
	}
	return c
}

// join is the clauses of the given kinds that apply, in that order, as one
// line's text.
func (p Palette) join(c clauses, kinds ...clauseKind) string {
	var parts []string
	for _, k := range kinds {
		if c[k] != "" {
			parts = append(parts, c[k])
		}
	}
	return strings.Join(parts, p.Dim+" · "+p.Rst)
}

// Age is the one relative-time vocabulary: the board's "observed … ago", the
// session picker's row stamps and the launch surfaces' figure ages all say
// how old something is the same way.
func Age(sec int64) string {
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

// labelWidth is the label column width for a set of views: wide enough for
// the longest label anywhere, so every account's bars align, and never
// narrower than the classic column.
func labelWidth(views []accountstate.Facts) int {
	w := minLabelWidth
	for _, v := range views {
		if v.Obs == nil {
			continue
		}
		for _, r := range v.Obs.Rows {
			if n := Cells(Sanitize(r.Label)); n > w {
				w = n
			}
		}
	}
	return w
}

// accountBlock renders the three axes independently, because they are three
// facts: what is wrong with the account (if anything), what is known about its
// quota, and how the last refresh went. A failed attempt annotates figures
// rather than deleting them, and a health problem is stated whether or not
// figures exist. The clauses are the compact row's; the block places them by
// narrative — the provenance on one line, the load on its own.
func (p Palette) accountBlock(v accountstate.Facts, now int64, width int) []string {
	c := p.clauses(v, now, false)
	lines := []string{p.headerLine(v)}
	line := func(prefix string, kinds ...clauseKind) {
		if s := p.join(c, kinds...); s != "" {
			lines = append(lines, "  "+prefix+s)
		}
	}
	line("", clauseHealth)
	line("", clauseEmpty)
	if v.Obs != nil {
		for _, r := range v.Obs.Rows {
			lines = append(lines, p.limitRow(r, now, width))
		}
	}
	line("", clauseAllowance)
	line("", clauseFeature)
	line("", clauseDrift)
	line("", clauseStale, clauseAge, clauseSource, clauseAttempt)
	line(p.Dim+"sessions: "+p.Rst, clauseLoad)
	return lines
}

// loadText is the load clause: the account's busy sessions and recent
// launches, from every home that shares the ledger, with another home's share
// named — "3 busy, 1 launched (steward-home: 2 busy, 1 launched)". "" when
// there are none, or the surface did not read them: an idle account says
// nothing, as it always has. It explains a mark rather than changing a
// choice, so it is the last clause of a caption.
func loadText(l *accountstate.Load) string {
	if l == nil || l.Busy+l.Launched == 0 {
		return ""
	}
	t := loadCounts(l.Busy, l.Launched)
	var others []string
	for _, h := range l.Elsewhere() {
		if c := loadCounts(h.Busy, h.Launched); c != "" {
			others = append(others, Sanitize(h.Label)+": "+c)
		}
	}
	if len(others) > 0 {
		t += " (" + strings.Join(others, "; ") + ")"
	}
	return t
}

func loadCounts(busy, launched int) string {
	var parts []string
	if busy > 0 {
		parts = append(parts, fmt.Sprintf("%d busy", busy))
	}
	if launched > 0 {
		parts = append(parts, fmt.Sprintf("%d launched", launched))
	}
	return strings.Join(parts, ", ")
}

// A limit row's display state, in two layers that degrade independently:
// what its percent can say, and what its reset can. Both layouts draw the
// same state — the block as a bar and a phrase, the compact row as a cell —
// so the state is decided once, by rowState, and each layout only spells it.
// Deciding it per layout is how the two came to disagree: a percent that
// failed to parse over a window that had ended read "resetting…" in the
// block and a negative clock in the compact row.
type pctState uint8

const (
	pctFigure      pctState = iota // a number that answers "how much is left"
	pctUnparseable                 // present and no longer a number: drift
	pctRolled                      // parsed, but about a window that has ended
)

type resetState uint8

const (
	resetAt          resetState = iota // a future instant: a clock
	resetAbsent                        // legitimately none — a window nobody has opened
	resetUnparseable                   // present and no longer parses: drift
	resetUnstarted                     // Codex: nobody has spent; the vendor's instant slides
	resetRolled                        // the instant has passed: the window ended
)

// rowState classifies a row once, with one precedence. The window having
// ended outranks every other reset reading, because a passed instant is a
// fact about the window whatever else the row says, and an unstarted window
// is one that has not ended. A percent that failed to parse outranks the
// window having ended — drift is the louder fact and the one `check` must
// see — while the reset layer still says the window ended, so neither layer
// draws a clock for a window that is over.
func rowState(r usage.Row, now int64) (pctState, resetState) {
	reset := resetAt
	switch {
	case r.RolledOver(now):
		reset = resetRolled
	case r.ResetState == tag.Bad:
		reset = resetUnparseable
	case r.Unstarted:
		reset = resetUnstarted
	case r.ResetAt == 0:
		reset = resetAbsent
	}
	switch {
	case r.PercentState == tag.Bad:
		return pctUnparseable, reset
	case reset == resetRolled:
		return pctRolled, reset
	}
	return pctFigure, reset
}

// limitRow: `  <label>  [██████░░░░...]  56%  resets Wed 18:00 (in 4.8d)`
// Bars and percentages retain their severity colour regardless of observation
// age; the provenance caption carries staleness separately. width is the
// board's label column (labelWidth), shared by every account.
func (p Palette) limitRow(r usage.Row, now int64, width int) string {
	pct, reset := rowState(r, now)
	glyphs, figure := bar(r.Percent), fmt.Sprintf("%3d%%", r.Percent)
	switch pct {
	case pctUnparseable:
		// A percent that no longer parses must not read as real headroom.
		glyphs, figure = strings.Repeat("?", barWidth), "  ?%"
	case pctRolled:
		// The window ended; whatever is being spent against the new one is
		// unknown. Showing the old number here is the same lie as showing a
		// drifted one — smaller, and in the direction that invites a choice.
		glyphs, figure = strings.Repeat("·", barWidth), "  ?%"
	}
	var phrase string
	switch reset {
	case resetAt:
		phrase = resetPhrase(r.ResetAt, now)
	case resetUnstarted:
		// Nobody has spent against this window yet, so the vendor's reset
		// instant slides with every request and means nothing.
		phrase = "not started"
	case resetRolled:
		phrase = "window rolled over"
	default:
		// Absent or unparseable: the drift marker is what tells them apart.
		phrase = "resets ?"
	}
	// The label carries the vendor's model name: sanitized, and padded by
	// cells so a wide one keeps every account's bars in one column.
	line := fmt.Sprintf("  %s %s[%s] %s%s  %s%s%s",
		PadCell(Sanitize(r.Label), width), p.severity(r, pct), glyphs, figure, p.Rst, p.Dim, phrase, p.Rst)
	if r.Drifted() {
		line += "  " + p.Red + "⚠ drift — run headroom check" + p.Rst
	}
	return line
}

// severity is the one colour rule for bars and percentages in both layouts.
// Observation age does not change severity. A percent that failed to parse
// is red, and a rolled-over window is dim whatever it once read.
func (p Palette) severity(r usage.Row, pct pctState) string {
	switch {
	case pct == pctUnparseable:
		return p.Red
	case pct == pctRolled:
		return p.Dim
	case r.Percent >= 80 || r.Severity != "normal":
		return p.Red
	case r.Percent >= 50:
		return p.Yel
	}
	return p.Grn
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

// bar is a percent as barWidth glyphs, clamped at both ends: a vendor
// percent below zero or above a hundred still draws a whole bar.
func bar(pct int) string {
	filled := max(min((pct*barWidth+50)/100, barWidth), 0)
	return strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
}

// resetPhrase is the block's clock for a reset still ahead — the one reset
// state that has one: the instant, with the weekday once it is a day or more
// away, and the time left.
func resetPhrase(resetAt, now int64) string {
	rem := resetAt - now
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
// than 24.0h. It is a remainder still to run: here only a reset rowState
// found ahead is given one — a reset that has passed is a rolled-over
// window, which has no clock — and placement's callers pass a window's time
// left.
func Remaining(rem int64) string {
	if h := float64(rem) / 3600; math.Round(h*10)/10 < 24 {
		return fmt.Sprintf("%.1fh", h)
	}
	return fmt.Sprintf("%.1fd", float64(rem)/86400)
}
