package app

// The evaluation surface: how automatic placement has done, from the two logs
// headroom keeps and never routes by. It reads them, the ledger's record of
// which homes spend against it and each home's discovered accounts — the disk
// alone, never the network — and writes nothing.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/eval"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usagelog"
)

// EvalSchema versions the --eval --json document.
const EvalSchema = 1

// evalListed is how many of each kind of evidence the text report lists.
const evalListed = 8

// parseSince reads --since: a span back from now ("7d", "36h", "90m") or an
// instant ("2026-10-01", or RFC3339).
func parseSince(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d >= 0 {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a span (7d, 36h) nor a date (2026-10-01)", s)
}

// gatherEval reads what one vendor's report is made from: every launch log of
// every home spending against this home's ledger, the usage log beside that
// ledger, and the subscription each home's account names stand for today.
// What cannot be read is named in the input, so the report says it is
// partial; only this home's own logs failing refuses the report.
func gatherEval(scope config.Scope, since, now time.Time) (eval.Input, error) {
	in := eval.Input{Vendor: scope.Vendor, Home: filepath.Clean(scope.AccountsRoot), Since: since, Now: now, Keys: map[string]string{}}
	st := state.Open(scope)
	snap := st.Load()
	for _, p := range snap.Problems() {
		in.Problems = append(in.Problems, fmt.Sprintf("state.json %s: %s — the homes on the ledger may be incomplete", p.Section, p.Detail))
	}

	known := func(root string, set accounts.Set) {
		for _, a := range set.Accounts {
			if a.AccountID != "" {
				in.Keys[root+"\x00"+a.Name] = state.Key{UUID: a.AccountID, Name: a.Name}.ID()
			}
		}
	}
	known(in.Home, accounts.Discover(scope))
	homes := []string{in.Home}
	for _, o := range otherHomes(scope, snap) {
		homes = append(homes, o.Root)
		known(o.Root, o.Set)
	}
	// A registered home whose own .ledger no longer names this one still
	// logged its launches against it while it did.
	for _, m := range snap.Members() {
		if root := filepath.Clean(m.Root); !slices.Contains(homes, root) {
			homes = append(homes, root)
		}
	}
	for _, home := range homes {
		recs, skipped, err := launchlog.Read(home, 0)
		if err != nil {
			if home == in.Home {
				return in, err
			}
			in.Problems = append(in.Problems, fmt.Sprintf("%s: %v — that home's launches are missing", launchlog.Path(home), err))
			continue
		}
		in.Skipped += skipped
		for _, r := range recs {
			in.Launches = append(in.Launches, eval.Launch{Home: home, Record: r})
		}
	}
	usage, skipped, err := usagelog.Read(st.Ledger())
	if err != nil {
		return in, err
	}
	in.Usage, in.Skipped = usage, in.Skipped+skipped
	return in, nil
}

func runEval(w io.Writer, scopes []config.Scope, since time.Time, jsonMode bool, now time.Time) int {
	var reports []eval.Report
	for _, scope := range scopes {
		in, err := gatherEval(scope, since, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom launches: %v\n", err)
			return 1
		}
		reports = append(reports, eval.Build(in))
	}
	if jsonMode {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		doc := struct {
			Schema      int           `json:"schema"`
			GeneratedAt string        `json:"generated_at"`
			Vendors     []eval.Report `json:"vendors"`
		}{EvalSchema, now.UTC().Format(time.RFC3339), reports}
		if err := enc.Encode(doc); err != nil {
			fmt.Fprintf(os.Stderr, "headroom launches: %v\n", err)
			return 1
		}
		return 0
	}
	for i, rep := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeEval(w, rep)
	}
	return 0
}

// writeEval is one vendor's report for a person: a column per rule version
// and one for what no rule placed, the machine's time beneath, then the
// evidence behind the figures, newest first.
func writeEval(w io.Writer, rep eval.Report) {
	src := rep.Sources
	span := "nothing recorded yet"
	if src.First != nil {
		span = evalWhen(*src.First) + " → " + evalWhen(*src.Last)
	}
	if rep.Since != nil {
		span = "since " + evalWhen(*rep.Since) + " · " + span
	}
	fmt.Fprintf(w, "%s placement · %s\n", rep.Vendor, span)
	fmt.Fprintf(w, "  read %d launch line(s) from %d home(s), %d usage-log reading(s), %d reading(s) carried in launch lines",
		src.Launches, src.Homes, src.Logged, src.Carried)
	if src.Skipped > 0 {
		fmt.Fprintf(w, ", %d unreadable line(s) skipped", src.Skipped)
	}
	fmt.Fprintln(w)
	for _, p := range src.Problems {
		fmt.Fprintln(w, "  partial: "+render.Sanitize(p))
	}
	if src.Unkeyed > 0 {
		fmt.Fprintf(w, "  %d account name(s) in older launch lines map to no subscription and count on their own\n", src.Unkeyed)
	}
	t := rep.Thresholds
	fmt.Fprintf(w, "  replay runs %s · near a limit at %d%% · room below %d%% on a reading under %s old, or a window ended since\n",
		rep.Rule, t.NearPercent, t.RoomBelow, render.Age(t.FreshForS))
	var faults, differ []eval.Decision
	for _, d := range rep.Decisions {
		switch {
		case d.Replay.Fault:
			faults = append(faults, d)
		case !d.Replay.Agrees:
			differ = append(differ, d)
		}
	}
	// Said before the table, because it undoes the table: a rule that no
	// longer decides its own lines alike is two rules under one name, and
	// every figure in its column mixes them.
	if len(faults) > 0 {
		fmt.Fprintf(w, "  fault: %d line(s) %s decided, with their whole input on record, %s now decides differently — a line is missing an input the rule reads, or the rule changed without a new name (placement.Rule); its column mixes both. See replay faults below.\n",
			len(faults), rep.Rule, rep.Rule)
	}
	if len(rep.Rules) == 0 {
		return
	}

	// A column per rule, and one for the windows no automatic launch went
	// into, when there were any. The rows about decisions have nothing to say
	// in that column.
	cols := rep.Rules
	u := rep.Unplaced
	unplaced := u.Session.ReachedNear+u.Weekly.ReachedNear+u.Weekly.Renewals > 0
	if unplaced {
		cols = append(slices.Clone(cols), u)
	}
	table := [][]string{{""}}
	row := func(decided bool, label string, cell func(eval.RuleSummary) string) {
		r := []string{label}
		for i, s := range cols {
			if decided && unplaced && i == len(cols)-1 {
				r = append(r, "—")
				continue
			}
			r = append(r, cell(s))
		}
		table = append(table, r)
	}
	decided := func(label string, cell func(eval.RuleSummary) string) { row(true, label, cell) }
	observed := func(label string, cell func(eval.RuleSummary) string) { row(false, label, cell) }
	for _, s := range cols {
		name := s.Rule
		if s.Rule == "" {
			name = "unplaced"
		}
		table[0] = append(table[0], name)
	}
	decided("launch lines", func(s eval.RuleSummary) string { return evalDay(s.From) + " → " + evalDay(s.To) })
	decided("launches (automatic)", func(s eval.RuleSummary) string {
		return fmt.Sprintf("%d (%d)", s.Launches.Total, s.Launches.Automatic)
	})
	decided("chosen on figures >"+render.Age(t.StaleAfterS)+" old", func(s eval.RuleSummary) string {
		l := s.Launches
		out := fmt.Sprintf("%d", l.ChosenStale)
		if l.ChosenUnobserved > 0 {
			out += fmt.Sprintf(" + %d never", l.ChosenUnobserved)
		}
		return out
	})
	decided("  chosen figures' age p50/p90", func(s eval.RuleSummary) string {
		if s.Launches.Automatic == 0 {
			return "—"
		}
		return render.Age(s.Launches.ChosenAgeP50S) + " / " + render.Age(s.Launches.ChosenAgeP90S)
	})
	decided("longest run on one account", func(s eval.RuleSummary) string { return strconv.Itoa(s.Launches.LongestRun) })
	decided("after launch: session ≥near", func(s eval.RuleSummary) string {
		return fmt.Sprintf("%d of %d", s.Outcomes.ReachedNear, s.Outcomes.Followed)
	})
	decided("  runner-up ≥near", func(s eval.RuleSummary) string {
		return fmt.Sprintf("%d of %d", s.Outcomes.RunnerUpNear, s.Outcomes.RunnerUpFollowed)
	})
	decided("  mean rise to the peak", func(s eval.RuleSummary) string {
		if s.Outcomes.Followed == 0 {
			return "—"
		}
		return fmt.Sprintf("%+.1f pts", s.Outcomes.MeanRise)
	})
	observed("session windows ≥near / full", func(s eval.RuleSummary) string {
		return fmt.Sprintf("%d / %d", s.Session.ReachedNear, s.Session.Exhausted)
	})
	observed("  time near, while another had room", func(s eval.RuleSummary) string {
		return evalSpan(s.Session.NearS) + ", " + evalSpan(s.Session.SqueezedS)
	})
	observed("weekly windows ≥near / full", func(s eval.RuleSummary) string {
		return fmt.Sprintf("%d / %d", s.Weekly.ReachedNear, s.Weekly.Exhausted)
	})
	observed("weekly renewals (mean used ≥)", func(s eval.RuleSummary) string {
		if s.Weekly.Renewals == 0 {
			return "0"
		}
		return fmt.Sprintf("%d (%.0f%%)", s.Weekly.Renewals, s.Weekly.MeanUsedMin)
	})
	observed("  points left unspent, at most", func(s eval.RuleSummary) string { return strconv.Itoa(s.Weekly.LapsedMaxPts) })
	observed("  … while another was ≥near", func(s eval.RuleSummary) string { return strconv.Itoa(s.Weekly.LapsedWhileSqueezed) })
	decided("replay agrees", func(s eval.RuleSummary) string {
		r := s.Replay
		out := fmt.Sprintf("%d of %d", r.Agreed, r.Replayed)
		var notes []string
		if r.Inexact > 0 {
			notes = append(notes, fmt.Sprintf("%d inexact", r.Inexact))
		}
		if r.Faults > 0 {
			notes = append(notes, fmt.Sprintf("%d fault", r.Faults))
		}
		if len(notes) > 0 {
			out += " (" + strings.Join(notes, ", ") + ")"
		}
		return out
	})
	fmt.Fprintln(w)
	writeTable(w, table)
	fmt.Fprintln(w, "  a window counts under the rule whose automatic launch went into it")
	if unplaced {
		fmt.Fprintln(w, "  unplaced: windows no automatic launch went into — pinned, named or resumed work")
	}
	f := rep.Fleet
	fmt.Fprintf(w, "  every account, owned by no rule: %s observed · a session ≥near %s, %s of it while another had room · mean session spread %.0f pts",
		evalSpan(f.ObservedS), evalSpan(f.NearS), evalSpan(f.SqueezedS), f.MeanSpreadPts)
	if f.UnobservedRenewals > 0 {
		fmt.Fprintf(w, " · %d weekly renewal(s) nobody read", f.UnobservedRenewals)
	}
	fmt.Fprintln(w)

	accts := map[string]bool{}
	for _, s := range rep.Rules {
		for a := range s.Launches.ByAccount {
			accts[a] = true
		}
	}
	if len(accts) > 0 {
		names := make([]string, 0, len(accts))
		for a := range accts {
			names = append(names, a)
		}
		slices.Sort(names)
		by := [][]string{{"automatic launches by account"}}
		for _, s := range rep.Rules {
			by[0] = append(by[0], s.Rule)
		}
		for _, a := range names {
			row := []string{"  " + render.Sanitize(a)}
			for _, s := range rep.Rules {
				row = append(row, strconv.Itoa(s.Launches.ByAccount[a]))
			}
			by = append(by, row)
		}
		fmt.Fprintln(w)
		writeTable(w, by)
	}

	writeEvidence(w, "windows that reached "+strconv.Itoa(t.NearPercent)+"%", len(rep.Episodes), func(yield func(string) bool) {
		for i := len(rep.Episodes) - 1; i >= 0; i-- {
			if !yield(episodeLine(rep.Episodes[i], rep.Home)) {
				return
			}
		}
	})
	writeEvidence(w, "weekly renewals", len(rep.Renewals), func(yield func(string) bool) {
		for i := len(rep.Renewals) - 1; i >= 0; i-- {
			if !yield(renewalLine(rep.Renewals[i])) {
				return
			}
		}
	})
	writeEvidence(w, "replay faults — lines "+rep.Rule+" decided and now decides differently", len(faults), func(yield func(string) bool) {
		for i := len(faults) - 1; i >= 0; i-- {
			if !yield(disagreementLine(faults[i], rep.Rule, rep.Home)) {
				return
			}
		}
	})
	writeEvidence(w, "replay disagreements — what "+rep.Rule+" would choose from the same input; each account's window as observed, not as the other choice would have left it",
		len(differ), func(yield func(string) bool) {
			for i := len(differ) - 1; i >= 0; i-- {
				if !yield(disagreementLine(differ[i], rep.Rule, rep.Home)) {
					return
				}
			}
		})
	fmt.Fprintln(w, "\n  --json carries every decision, window and renewal behind these figures.")
}

func writeEvidence(w io.Writer, title string, n int, lines func(func(string) bool)) {
	if n == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %s", title)
	if n > evalListed {
		fmt.Fprintf(w, " (newest %d of %d)", evalListed, n)
	}
	fmt.Fprintln(w)
	shown := 0
	lines(func(s string) bool {
		fmt.Fprintln(w, "    "+s)
		shown++
		return shown < evalListed
	})
}

func episodeLine(ep eval.Episode, home string) string {
	line := evalWhen(ep.NearAt) + "  " + render.Sanitize(ep.Account) + "  " + render.Sanitize(ep.Limit) + fmt.Sprintf(" peak %d%%", ep.Peak)
	if ep.ExhaustedAt != nil {
		line += " (full at " + evalWhen(*ep.ExhaustedAt) + ")"
	}
	line += fmt.Sprintf(" · near %s, %s while another had room  %s", evalSpan(ep.NearS), evalSpan(ep.SqueezedS), evalCharged(ep.Rule))
	if len(ep.Launches) > 0 {
		var parts []string
		for _, mode := range []string{"auto", "pinned", "named", "last", "picker"} {
			if n := ep.Launches[mode]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, mode))
			}
		}
		line += " · launched there: " + strings.Join(parts, ", ")
		if from := otherHomesSaid(ep.ByHome, home); from != "" {
			line += " (" + from + ")"
		}
	}
	if r := ep.Elsewhere; r != nil {
		line += fmt.Sprintf(" · room on %s (session %d%%, %s)", render.Sanitize(r.Account), r.Session, roomBasis(*r))
	} else {
		line += " · no room elsewhere on the readings"
	}
	return line
}

// roomBasis says what another account's room rests on.
func roomBasis(r eval.Room) string {
	if r.Basis == "ended" {
		return "its window ended since a reading " + render.Age(r.AgeS) + " old"
	}
	if r.AgeS < 1 {
		return "read at that moment"
	}
	return "read " + render.Age(r.AgeS) + " before"
}

func renewalLine(r eval.Renewal) string {
	line := evalWhen(r.RenewedAt) + "  " + render.Sanitize(r.Account) + "  " + render.Sanitize(r.Limit) +
		fmt.Sprintf(" used ≥%d%%, read %s before", r.UsedMin, render.Age(r.LastReadS))
	if r.PeakElsewhere > 0 {
		line += fmt.Sprintf(" · elsewhere peaked %d%%", r.PeakElsewhere)
	}
	return line + "  " + evalCharged(r.Rule)
}

// evalCharged says which column a window counted in.
func evalCharged(rule string) string {
	if rule == "" {
		return "unplaced"
	}
	return rule
}

func disagreementLine(d eval.Decision, rule, home string) string {
	replayed := d.Replay.Chosen
	if replayed == "" {
		replayed = "(refuse)"
	}
	where := ""
	if d.Home != home {
		where = " on " + render.Sanitize(evalHomeLabel(d.Home))
	}
	line := fmt.Sprintf("%s  %s%s chose %s (%s); %s chooses %s (%s)", evalWhen(d.At), d.Rule, where, render.Sanitize(d.Chosen), d.Reason,
		rule, render.Sanitize(replayed), d.Replay.Reason)
	var after []string
	if a := d.ChosenAfter; a != nil {
		after = append(after, fmt.Sprintf("%s %d%%→%d%%", render.Sanitize(d.Chosen), a.Counted, a.Peak))
	}
	if a := d.Replay.After; a != nil {
		after = append(after, fmt.Sprintf("%s %d%%→%d%%", render.Sanitize(replayed), a.Counted, a.Peak))
	}
	if len(after) > 0 {
		line += " · observed " + strings.Join(after, ", ")
	}
	if !d.Replay.Exact {
		line += " · inexact"
	}
	return line
}

// otherHomesSaid is how many of some launches other homes made: "12 from
// steward-home". "" when this home made them all.
func otherHomesSaid(byHome map[string]int, home string) string {
	var roots []string
	for root := range byHome {
		if root != home {
			roots = append(roots, root)
		}
	}
	slices.Sort(roots)
	var parts []string
	for _, root := range roots {
		parts = append(parts, fmt.Sprintf("%d from %s", byHome[root], render.Sanitize(evalHomeLabel(root))))
	}
	return strings.Join(parts, ", ")
}

// evalHomeLabel names a home by its accounts root, as the launch line does
// for a home with no registration to name it.
func evalHomeLabel(root string) string { return homeLabel(nil, root) }

// writeTable prints rows as aligned columns, two spaces apart.
func writeTable(w io.Writer, rows [][]string) {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], render.Cells(c))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range r {
			if i == len(r)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(render.PadCell(c, widths[i]) + "  ")
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

func evalWhen(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("01-02 15:04")
	}
	return s
}

func evalDay(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("01-02")
	}
	return s
}

// evalSpan is a length of time for the table: hours and minutes.
func evalSpan(sec int64) string {
	if sec <= 0 {
		return "0m"
	}
	h, m := sec/3600, sec%3600/60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}
