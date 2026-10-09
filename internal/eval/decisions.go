package eval

// Each automatic launch: what it was decided on, what its account's session
// window did afterwards, and what this binary's rule decides from the same
// input. A decision counts under its own line's rule, whichever home made it.

import (
	"slices"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
)

func (e *evaluator) decisions(rep *Report) {
	ages := map[string][]int64{}
	rise := map[string][2]float64{} // sum, count
	runKey, runRule, run := "", "", 0
	for _, l := range e.launches {
		if l.at < e.since {
			continue
		}
		s := e.rule(l.Rule)
		s.Launches.Total++
		mode := l.Mode
		if l.automatic {
			mode = "auto"
		}
		s.Launches.ByMode[mode]++
		if !l.automatic {
			continue
		}
		s.Launches.Automatic++
		s.Launches.ByReason[l.Reason]++
		s.Launches.ByAccount[e.display(l.chosenKey)]++
		if l.chosenKey == runKey && l.Rule == runRule {
			run++
		} else {
			runKey, runRule, run = l.chosenKey, l.Rule, 1
		}
		s.Launches.LongestRun = max(s.Launches.LongestRun, run)

		d := Decision{At: l.At, Home: l.Home, Rule: l.Rule, Mode: l.Mode, Reason: l.Reason, Chosen: l.Chosen, RunnerUp: l.RunnerUp}
		if c, ok := find(l.Record, l.Chosen); ok {
			if c.ObservedAt == nil {
				s.Launches.ChosenUnobserved++
			} else if age := l.at - unix(c.ObservedAt); age >= 0 {
				ages[l.Rule] = append(ages[l.Rule], age)
				if age > int64(placement.StaleAfter/time.Second) {
					s.Launches.ChosenStale++
				}
			}
			d.ChosenAfter = e.after(l.chosenKey, l.at, c)
		}
		if a := d.ChosenAfter; a != nil {
			s.Outcomes.Followed++
			if a.Peak >= NearPercent {
				s.Outcomes.ReachedNear++
			}
			if a.Peak >= FullPercent {
				s.Outcomes.Exhausted++
			}
			r := rise[l.Rule]
			rise[l.Rule] = [2]float64{r[0] + float64(a.Peak-a.Counted), r[1] + 1}
		}
		if c, ok := find(l.Record, l.RunnerUp); ok && l.RunnerUp != "" {
			if a := e.after(e.keyOrName(l.Home, c), l.at, c); a != nil {
				d.RunnerUpAfter = a
				s.Outcomes.RunnerUpFollowed++
				if a.Peak >= NearPercent {
					s.Outcomes.RunnerUpNear++
				}
			}
		}
		d.Replay = e.replay(l)
		s.Replay.Replayed++
		if d.Replay.Agrees {
			s.Replay.Agreed++
		}
		if !d.Replay.Exact {
			s.Replay.Inexact++
		}
		rep.Decisions = append(rep.Decisions, d)
	}
	for rule, as := range ages {
		s := e.rule(rule)
		slices.Sort(as)
		s.Launches.ChosenAgeP50S = as[len(as)/2]
		s.Launches.ChosenAgeP90S = as[min(len(as)-1, len(as)*9/10)]
	}
	for rule, r := range rise {
		e.rule(rule).Outcomes.MeanRise = round1(r[0] / r[1])
	}
}

func find(r launchlog.Record, name string) (launchlog.Candidate, bool) {
	for _, c := range r.Candidates {
		if c.Name == name {
			return c, true
		}
	}
	return launchlog.Candidate{}, false
}

// after follows an account's session window from a launch, for Follow: the
// window the launch went into, and how high it went once the launch was made.
// The figure the launch was decided on counts as the window's starting point
// only when it was a reading of this window; a window that opened after the
// launch started from nothing. Nil when nobody read the window in that time.
func (e *evaluator) after(key string, at int64, c launchlog.Candidate) *After {
	w := e.sessionAt(key, at)
	if w == nil {
		return nil
	}
	end := at + int64(Follow/time.Second)
	var out *After
	for _, p := range w.points {
		if p.at <= at || p.at > end {
			continue // what the launch was decided on, or past what is followed
		}
		if out == nil || p.percent > out.Peak {
			out = &After{Peak: p.percent, PeakAt: stamp(p.at), ResetsAt: stamp(w.reset)}
		}
	}
	if out == nil {
		return nil
	}
	for _, l := range c.Limits {
		if l.Session && minute(unix(l.ResetsAt)) == w.reset {
			out.Counted = l.Counted
		}
	}
	return out
}

// ---- the replay ----

// replay decides a launch line again with this binary's rule. A version 2 line
// carries the rule's whole input — the candidates, their busy processes, the
// record of launches and its last placements to the millisecond — and is
// rebuilt as it was read. An older line carries busy sessions and recent
// launches only as the counts the rule made of them; it is rebuilt from those,
// which reproduces a rule that counts alike and nothing more, and is inexact
// wherever such counts, a tie inside one second, or the intent decided.
func (e *evaluator) replay(l launch) Replay {
	now := time.Unix(l.at, 0)
	if l.AtMS > 0 {
		now = time.UnixMilli(l.AtMS)
	}
	whole := l.Recent != nil
	for _, c := range l.Candidates {
		whole = whole && c.BusyProcs != nil
	}
	exact := true
	var cands []placement.Candidate
	ledger := placement.Ledger{Last: map[string]placement.Last{}}
	pending := map[string]int{} // older lines: the recent launches each subscription counted
	seconds := map[int64]int{}  // older lines: last placements that fall in one second
	pid := 0
	for _, c := range l.Candidates {
		key := e.keyOrName(l.Home, c)
		pc := placement.Candidate{Name: c.Name, Key: key, Excluded: c.Excluded, Source: c.Source, Statuses: c.Statuses}
		if !c.Eligible && pc.Excluded == "" {
			pc.Excluded = "excluded"
		}
		if c.ObservedAt != nil {
			pc.ObservedAt = unix(c.ObservedAt)
		}
		for _, lim := range c.Limits {
			pc.Limits = append(pc.Limits, placement.Limit{
				Kind: lim.Kind, Label: lim.Label, Percent: lim.Percent, Bad: lim.Basis == string(placement.BasisBad),
				ResetAt: unix(lim.ResetsAt), Window: lim.WindowS, Period: lim.PeriodS, Session: lim.Session,
			})
		}
		if whole {
			for _, p := range c.BusyProcs {
				pc.Busy = append(pc.Busy, placement.Proc{PID: p.PID, StartedMS: p.StartedMS, Home: p.Home})
			}
		} else {
			// The busy count, as processes no recorded launch carries, so the
			// rule's deduplication finds nothing to merge: the line did.
			for range c.Busy {
				pid--
				pc.Busy = append(pc.Busy, placement.Proc{PID: pid})
			}
			pending[key] = max(pending[key], c.Pending)
			exact = exact && c.Busy == 0 && c.Pending == 0
		}
		if t, ok := parse(c.LastPlaced); ok {
			if cur, seen := ledger.Last[key]; !seen || t.UnixMilli() > cur.AtMS {
				ledger.Last[key] = placement.Last{Name: c.Name, AtMS: t.UnixMilli()}
			}
			if !strings.Contains(*c.LastPlaced, ".") {
				seconds[t.Unix()]++
			}
		}
		cands = append(cands, pc)
	}
	if whole {
		ledger.Recent = slices.Clone(l.Recent)
		// A line without its millisecond decided at a second the replay can
		// only round, which a recent launch on the edge of PendingFor feels.
		exact = exact && l.AtMS > 0
	} else {
		// Two dirs on one subscription each counted the same launches: they
		// go back once, for the subscription.
		for _, c := range cands {
			for ; pending[c.Key] > 0; pending[c.Key]-- {
				ledger.Recent = append(ledger.Recent, placement.Pending{Key: c.Key, Name: c.Name, AtMS: now.UnixMilli()})
			}
		}
	}
	for _, n := range seconds {
		exact = exact && n < 2
	}
	intent := placement.Intent{Kind: placement.Auto, Home: l.Home, Owner: l.Owner, Exclude: l.Exclude}
	if l.V < 2 {
		// Version 1 kept no intent: an owner is known only when it was
		// followed, and an account the picker moved a session off not at all.
		if l.Reason == placement.ReasonOwner {
			intent.Owner = l.Chosen
		}
		exact = exact && l.Mode == "auto" && l.Reason != placement.ReasonMoved
	}
	d := placement.Choose(cands, ledger, intent, now)
	out := Replay{Chosen: d.Chosen, Reason: d.Reason, Agrees: d.Chosen == l.Chosen, Exact: exact}
	if !out.Agrees && d.Chosen != "" {
		if c, ok := find(l.Record, d.Chosen); ok {
			out.After = e.after(e.keyOrName(l.Home, c), l.at, c)
		}
	}
	return out
}
