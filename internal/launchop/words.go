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
	"github.com/qiushiyan/headroom/internal/state"
)

// launchLine is the one line a launch prints before the vendor starts: the
// account, how it was decided, and — when the rule decided — the figures it
// decided on, each said as what it is. automatic says the rule chose; ref and
// owner are the session the launch is for and the account that last drove it;
// others is how much of the chosen account's load other homes put there.
func launchLine(d placement.Decision, automatic bool, ref Session, owner, others string, now time.Time) string {
	parts := []string{d.Chosen}
	c, _ := d.Find(d.Chosen)
	if !automatic {
		switch d.Reason {
		case "pinned":
			parts = append(parts, "pinned (a on the board turns auto on)")
		case placement.ReasonLast:
			parts = append(parts, "the last account used")
		case placement.ReasonOwner:
			parts = append(parts, "this session's account")
		default:
			parts = append(parts, d.Reason)
		}
		return strings.Join(parts, " · ")
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
		load := fmt.Sprintf("load %d", c.Load)
		if others != "" {
			load += " (" + others + ")"
		}
		parts = append(parts, load)
	}
	line := strings.Join(parts, " · ")
	if d.RunnerUp != "" {
		line += " (next: " + d.RunnerUp + ")"
	}
	return line
}

// elsewhere says how much of a counted account's busy sessions and recent
// launches came from homes other than this one, by name: "2 from
// steward-home". "" when none did.
func elsewhere(c placement.Counted, home string, labels state.HomeLabels) string {
	var parts []string
	for _, sh := range c.Elsewhere(home) {
		if n := sh.Busy + sh.Pending; n > 0 {
			parts = append(parts, fmt.Sprintf("%d from %s", n, render.Sanitize(labels.Of(sh.Home))))
		}
	}
	return strings.Join(parts, ", ")
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
// account, with what was counted and why an account was left out. busy and
// pending count every home sharing the ledger; a note says whose were not
// this home's.
func writeDryRun(w io.Writer, binary string, d placement.Decision, mode string, now time.Time, home string, labels state.HomeLabels) {
	if d.Chosen == "" {
		fmt.Fprintf(w, "headroom launch --dry-run: would refuse — %s\n", d.Refusal)
	} else {
		how := mode
		if d.Reason != "" && d.Reason != mode {
			how += " · " + d.Reason
		}
		fmt.Fprintf(w, "headroom launch --dry-run: would start %s on %s (%s)\n", binary, d.Chosen, how)
	}
	fmt.Fprintf(w, "rule %s — nothing was recorded, logged or started\n\n", d.Rule)

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
			session, week, WeekLeft(c.Week), c.Busy, c.Pending, c.Load, dryRunNote(c, now, home, labels))
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

func dryRunNote(c placement.Counted, now time.Time, home string, labels state.HomeLabels) string {
	var notes []string
	if c.Excluded != "" {
		notes = append(notes, "excluded: "+c.Excluded)
	}
	if c.Reservation == placement.ReservedHere {
		notes = append(notes, "reserved here")
	}
	for _, sh := range c.Elsewhere(home) {
		notes = append(notes, fmt.Sprintf("%s: %d busy, %d pending", render.Sanitize(labels.Of(sh.Home)), sh.Busy, sh.Pending))
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
