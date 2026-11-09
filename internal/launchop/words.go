package launchop

// How a launch says its choice: the one line every launch prints before the
// vendor starts, and the dry run's table. Each figure is said as what it rests
// on — measured, a lower bound from old figures, a window that has ended, a
// percent that did not parse — because the rule counts each differently, and
// a line that printed them alike would read a bound as measured room.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
)

// launchLine is the one line a launch prints before the vendor starts: the
// account, how it was decided, and — when the rule decided — the figures it
// decided on, each said as what it is. automatic says the rule chose; ref and
// owner are the session the launch is for and the account that last drove it.
func launchLine(d placement.Decision, automatic bool, ref Session, owner string, now time.Time) string {
	parts := []string{d.Chosen}
	c, _ := d.Find(d.Chosen)
	if !automatic {
		return strings.Join(append(parts, d.Reason), " · ")
	}
	parts = append(parts, "auto")
	switch {
	case ref.Unidentified:
		parts = append(parts, "session not identified")
	case ref.Route == "":
	case owner == "":
		parts = append(parts, "session had no known account")
	case owner == d.Chosen:
		parts = append(parts, "this session's account")
	default:
		parts = append(parts, "session moved from "+owner)
	}
	if d.Reason == placement.ReasonNearLimit {
		near := "every account is near a limit · " + shortLabel(c.HighestLabel) + " " + figure(c.Highest, c.HighestBasis)
		if c.HighestReset > now.Unix() {
			near += ", resets in " + render.Remaining(c.HighestReset-now.Unix())
		}
		parts = append(parts, near)
		if c.Stale {
			parts = append(parts, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old")
		}
	} else {
		parts = append(parts, figures(c, now)...)
		parts = append(parts, fmt.Sprintf("load %d", c.Load))
	}
	if left := expiredLogins(d); left != "" {
		parts = append(parts, left)
	}
	line := strings.Join(parts, " · ")
	if d.RunnerUp != "" {
		line += " (next: " + d.RunnerUp + ")"
	}
	return line
}

// expiredLogins says how many accounts the rule could not consider because
// their login has expired, and the command that renews them. An automatic
// launch is the line a person reads every day, and an account left out this
// way is quota nobody spends: the board and the dry run say so too, but only
// when they are opened.
func expiredLogins(d placement.Decision) string {
	n := 0
	for _, c := range d.Candidates {
		if c.Excluded == loginExpired {
			n++
		}
	}
	switch n {
	case 0:
		return ""
	case 1:
		return "1 account left out, login expired: run headroom login"
	}
	return fmt.Sprintf("%d accounts left out, login expired: run headroom login", n)
}

// figure is one counted percentage said as what it rests on: measured, a lower
// bound from an old observation, a window that has since ended, or a figure
// that did not parse. A bound printed as a bare percent reads as measured.
func figure(counted int, basis placement.Basis) string {
	switch basis {
	case placement.BasisStale:
		return fmt.Sprintf("≥%d%%", counted)
	case placement.BasisEnded:
		return "window ended"
	case placement.BasisBad:
		return "?%"
	default:
		return fmt.Sprintf("%d%%", counted)
	}
}

// figures is one account's session and weekly figures, and how old they are
// when that matters.
func figures(c placement.Counted, now time.Time) []string {
	if c.ObservedAt == 0 {
		return []string{"never observed"}
	}
	var out []string
	if s, ok := c.Session(); ok {
		out = append(out, shortLabel(s.Label)+" "+figure(s.Counted, s.Basis))
	}
	if c.Week.Basis != "" {
		week := "week " + figure(c.Week.Counted, c.Week.Basis)
		if c.Week.LeftBasis == placement.TimeReset {
			week += ", resets in " + render.Remaining(c.Week.Left)
		}
		out = append(out, week)
	}
	if len(c.Limits) == 0 {
		out = append(out, "no limits reported")
	}
	if c.Stale {
		out = append(out, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old")
	}
	return out
}

// shortLabel trims a row's label to what fits a one-line launch message.
func shortLabel(label string) string {
	label = strings.TrimSuffix(render.Sanitize(label), " session")
	if label == "" {
		return "limit"
	}
	return label
}

// writeDryRun prints the whole table a launch would choose from: one row per
// account, with what was counted and why an account was left out.
func writeDryRun(w io.Writer, binary string, d placement.Decision, mode string, now time.Time, f facts) {
	if d.Chosen == "" {
		fmt.Fprintf(w, "headroom launch --dry-run: would refuse — %s\n", d.Refusal)
	} else {
		how := mode
		if d.Reason != "" && d.Reason != mode {
			how += " · " + d.Reason
		}
		fmt.Fprintf(w, "headroom launch --dry-run: would start %s on %s (%s)\n", binary, d.Chosen, how)
	}
	fmt.Fprint(w, "nothing was recorded, logged or started\n\n")

	nameW := len("account")
	for _, c := range d.Candidates {
		nameW = max(nameW, render.Cells(render.Sanitize(c.Name)))
	}
	fmt.Fprintf(w, "  %s  %7s  %5s  %6s  %4s  %7s  %4s  %s\n", render.PadCell("account", nameW), "session", "week", "resets", "busy", "pending", "load", "note")
	for _, c := range d.Candidates {
		mark := "  "
		switch c.Name {
		case d.Chosen:
			mark = "→ "
		case d.RunnerUp:
			mark = "· "
		}
		session, week := "—", "—"
		if s, ok := c.Session(); ok {
			session = Cell(s.Counted, s.Basis)
		}
		if c.Week.Basis != "" {
			week = Cell(c.Week.Counted, c.Week.Basis)
		}
		fmt.Fprintf(w, "%s%s  %7s  %5s  %6s  %4d  %7d  %4d  %s\n", mark, render.PadCell(render.Sanitize(c.Name), nameW),
			session, week, WeekLeft(c.Week), c.Busy, c.Pending, c.Load, dryRunNote(c, now, f.standing(c.Name)))
	}
}

// WeekLeft is how long a counted week has left, said as what it rests on: the
// vendor's reset as a countdown, a renewal its schedule projects as an
// approximate one, and "—" for a window counted whole because nothing dates
// it — there is no instant to print.
func WeekLeft(w placement.Week) string {
	switch w.LeftBasis {
	case placement.TimeReset:
		return render.Remaining(w.Left)
	case placement.TimeProjected:
		return "≈" + render.Remaining(w.Left)
	default:
		return "—"
	}
}

// Cell is figure for a table column: the same distinctions, in fewer cells.
func Cell(counted int, basis placement.Basis) string {
	if basis == placement.BasisEnded {
		return "ended"
	}
	return figure(counted, basis)
}

func dryRunNote(c placement.Counted, now time.Time, standing string) string {
	var notes []string
	if c.Excluded != "" {
		notes = append(notes, "excluded: "+c.Excluded)
	}
	if standing == reservedHere {
		notes = append(notes, "reserved here")
	}
	if c.NearLimit {
		notes = append(notes, fmt.Sprintf("near a limit (%s %d%%)", shortLabel(c.HighestLabel), c.Highest))
	}
	switch {
	case c.ObservedAt == 0:
		notes = append(notes, "never observed")
	case c.Stale:
		notes = append(notes, "figures "+render.Age(now.Unix()-c.ObservedAt)+" old — lower bounds")
	default:
		notes = append(notes, "observed "+render.Age(now.Unix()-c.ObservedAt)+" ago")
	}
	for _, l := range c.Limits {
		if l.Basis == placement.BasisEnded {
			notes = append(notes, shortLabel(l.Label)+" window ended")
		}
	}
	return strings.Join(notes, " · ")
}
