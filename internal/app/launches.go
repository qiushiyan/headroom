package app

// The launches surface: what the launch log holds, for a person. It reads the
// log and nothing else, and nothing that routes reads the log at all. --eval
// is its other half (launches_eval.go): what followed the launches.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
)

const defaultLaunches = 20

func runLaunches(scopes []config.Scope, args []string) int {
	return runLaunchesTo(os.Stdout, scopes, args)
}

func runLaunchesTo(w io.Writer, scopes []config.Scope, args []string) int {
	n, jsonMode, evaluate, nSet := defaultLaunches, false, false, false
	var since time.Time
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonMode = true
		case "--eval":
			evaluate = true
		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom launches: --since needs a span or a date")
				return 2
			}
			i++
			t, err := parseSince(args[i], time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "headroom launches: %v\n", err)
				return 2
			}
			since = t
		case "-n":
			nSet = true
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom launches: -n needs a count")
				return 2
			}
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "headroom launches: -n %q is not a count\n", args[i])
				return 2
			}
			n = v // 0 = every record
		default:
			fmt.Fprintf(os.Stderr, "headroom launches: unknown argument %q\n", args[i])
			return 2
		}
	}
	switch {
	case evaluate && nSet:
		fmt.Fprintln(os.Stderr, "headroom launches: --eval reads every line — -n does not apply; --since narrows it")
		return 2
	case !evaluate && !since.IsZero():
		fmt.Fprintln(os.Stderr, "headroom launches: --since narrows --eval")
		return 2
	case evaluate:
		return runEval(w, scopes, since, jsonMode, time.Now())
	}

	var all []launchlog.Record
	skipped := 0
	for _, scope := range scopes {
		recs, bad, err := launchlog.Read(scope.AccountsRoot, n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom launches: %v\n", err)
			return 1
		}
		all = append(all, recs...)
		skipped += bad
	}
	// One list across vendors, by time: RFC3339 UTC sorts as text.
	sort.SliceStable(all, func(i, j int) bool { return all[i].At < all[j].At })
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}

	if jsonMode {
		enc := json.NewEncoder(w)
		for _, r := range all {
			if err := enc.Encode(r); err != nil {
				fmt.Fprintf(os.Stderr, "headroom launches: %v\n", err)
				return 1
			}
		}
		return 0
	}
	if len(all) == 0 {
		fmt.Fprintln(w, "no launches recorded yet")
		return 0
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
		fmt.Fprintf(os.Stderr, "headroom launches: %d unreadable line(s) skipped\n", skipped)
	}
	return 0
}

// launchesLine is one record on one line: when, where, how it was decided, the
// load it was decided on, and the account that would have been next.
func launchesLine(r launchlog.Record, nameW int, vendor bool) string {
	when := r.At
	if t, err := time.Parse(time.RFC3339, r.At); err == nil {
		when = t.Local().Format("01-02 15:04")
	}
	how := r.Mode
	if r.Reason != "" && r.Reason != r.Mode {
		how += "/" + r.Reason
	}
	line := when + "  "
	if vendor {
		line += render.PadCell(r.Vendor, 6) + "  "
	}
	line += render.PadCell(render.Sanitize(r.Chosen), nameW) + "  " + render.PadCell(how, 16)
	for _, c := range r.Candidates {
		if c.Name == r.Chosen {
			line += fmt.Sprintf("  load %d  week %3d%%", c.Load, c.Weekly)
			// How long that week had left at the launch: what decides
			// between equally loaded accounts, so a choice of the fuller one
			// reads as what it was. A record from before the field, or a
			// window nothing dated, says nothing.
			week := placement.Week{Left: c.Week.LeftS, LeftBasis: placement.TimeBasis(c.Week.LeftBasis)}
			if left := weekLeft(week); left != "—" {
				line += ", " + left + " left"
			}
			break
		}
	}
	if r.RunnerUp != "" {
		line += "  next " + render.Sanitize(r.RunnerUp)
	}
	if !r.Recorded {
		line += "  (not recorded)"
	}
	return line
}
