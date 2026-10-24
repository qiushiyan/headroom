package app

// The launches surface: what the launch log holds, for a person. It reads the
// log and nothing else, and nothing that routes reads the log at all. --eval
// is its other half (launches_eval.go): what followed the launches.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
)

// DefaultLaunches is how many of the newest launches the log lists unless
// asked for another count.
const DefaultLaunches = 20

// Launches lists the newest n launches across the vendors' logs, one line
// each — or every one, when n is 0 — or, with asJSON, one record per line.
func Launches(std Streams, scopes []config.Scope, n int, asJSON bool) error {
	w := std.Out
	var all []launchlog.Record
	skipped := 0
	for _, scope := range scopes {
		recs, bad, err := launchlog.Read(scope.AccountsRoot)
		if err != nil {
			return err
		}
		all = append(all, recs...)
		skipped += bad
	}
	// One list across vendors, by time — RFC3339 UTC sorts as text — and -n
	// keeps its newest.
	sort.SliceStable(all, func(i, j int) bool { return all[i].At < all[j].At })
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}

	if asJSON {
		enc := json.NewEncoder(w)
		for _, r := range all {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}
	if len(all) == 0 {
		fmt.Fprintln(w, "no launches recorded yet")
		return nil
	}
	multi := len(scopes) > 1
	nameW := 0
	for _, r := range all {
		nameW = max(nameW, render.Cells(render.Sanitize(r.Chosen)))
	}
	for _, r := range all {
		fmt.Fprintln(w, launchesLine(r, nameW, multi))
	}
	if skipped > 0 {
		fmt.Fprintf(std.Err, "headroom launches: %d unreadable line(s) skipped\n", skipped)
	}
	return nil
}

// launchesLine is one record on one line: when, where, how it was decided, the
// load it was decided on, and the account that would have been next.
func launchesLine(r launchlog.Record, nameW int, vendor bool) string {
	when := render.LogWhen(r.At)
	how := r.Mode
	if r.Reason != "" && r.Reason != r.Mode {
		how += "/" + r.Reason
	}
	var line strings.Builder
	line.WriteString(when + "  ")
	if vendor {
		line.WriteString(render.PadCell(r.Vendor, 6) + "  ")
	}
	line.WriteString(render.PadCell(render.Sanitize(r.Chosen), nameW) + "  " + render.PadCell(how, 16))
	for _, c := range r.Candidates {
		if c.Name == r.Chosen {
			line.WriteString(fmt.Sprintf("  load %d  week %3d%%", c.Load, c.Weekly))
			// How long that week had left at the launch: what decides
			// between equally loaded accounts, so a choice of the fuller one
			// reads as what it was. A record from before the field, or a
			// window nothing dated, says nothing.
			week := placement.Week{Left: c.Week.LeftS, LeftBasis: placement.TimeBasis(c.Week.LeftBasis)}
			if left := launchop.WeekLeft(week); left != "—" {
				line.WriteString(", " + left + " left")
			}
			break
		}
	}
	if r.RunnerUp != "" {
		line.WriteString("  next " + render.Sanitize(r.RunnerUp))
	}
	if !r.Recorded {
		line.WriteString("  (not recorded)")
	}
	return line.String()
}
