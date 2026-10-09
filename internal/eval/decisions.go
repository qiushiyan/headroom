package eval

// Each automatic launch: what it was decided on, what its account's session
// window did afterwards, and what this binary's rule decides from the same
// input. A decision counts under its own line's rule, whichever home made it.

import (
	"slices"
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

		d := Decision{At: l.At, Home: l.Home, Rule: l.Rule, Mode: l.Mode, Reason: l.Reason, Chosen: e.display(l.chosenKey)}
		if c, ok := find(l.Record, l.Chosen); ok {
			if c.ObservedAt == nil {
				s.Launches.ChosenUnobserved++
			} else if age := l.at - unix(c.ObservedAt); age >= 0 {
				ages[l.Rule] = append(ages[l.Rule], age)
				if age > int64(placement.StaleAfter/time.Second) {
					s.Launches.ChosenStale++
				}
			}
			d.ChosenAfter = e.after(l.chosenKey, l.at, sessionCounted(c))
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
			k := e.keyOrName(l.Home, c)
			d.RunnerUp = e.display(k)
			if a := e.after(k, l.at, sessionCounted(c)); a != nil {
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

// sessionCounted is the session figure a launch line counted for an account.
func sessionCounted(c launchlog.Candidate) int {
	for _, l := range c.Limits {
		if l.Session {
			return l.Counted
		}
	}
	return 0
}

// after follows an account's session window from a moment: the window the
// first reading after it describes, and how high that window went. A reading
// taken at the moment itself is what the launch was decided on, not what
// followed it.
func (e *evaluator) after(key string, at int64, countedAt int) *After {
	end := at + int64(Follow/time.Second)
	var reset int64
	var out *After
	for _, r := range e.readings[key] {
		if r.at <= at {
			continue
		}
		if r.at > end {
			break
		}
		for _, w := range r.rows {
			if !w.session || w.bad || w.reset <= r.at {
				continue
			}
			if reset == 0 {
				reset = minute(w.reset)
				out = &After{Counted: countedAt, Peak: w.percent, PeakAt: stamp(r.at), ResetsAt: stamp(reset)}
			}
			if minute(w.reset) == reset && w.percent > out.Peak {
				out.Peak, out.PeakAt = w.percent, stamp(r.at)
			}
		}
	}
	return out
}

// replay decides a launch line again with this binary's rule, from the input
// the line recorded. Busy sessions and recent launches come back as the
// counts the line holds — the rule's own deduplication already applied, so a
// rule that counts them differently is replayed on the old counting — and the
// line's time is its second.
func (e *evaluator) replay(l launch) Replay {
	now := time.Unix(l.at, 0)
	var cands []placement.Candidate
	ledger := placement.Ledger{Last: map[string]placement.Last{}}
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
		// Busy sessions get pids no recorded launch carries, so the rule's
		// deduplication finds nothing to merge: the line already did.
		for range c.Busy {
			pid--
			pc.Busy = append(pc.Busy, placement.Proc{PID: pid})
		}
		for range c.Pending {
			ledger.Recent = append(ledger.Recent, placement.Pending{Key: key, Name: c.Name, AtMS: now.UnixMilli()})
		}
		if t := unix(c.LastPlaced); t > 0 {
			if cur, ok := ledger.Last[key]; !ok || t*1000 > cur.AtMS {
				ledger.Last[key] = placement.Last{Name: c.Name, AtMS: t * 1000}
			}
		}
		cands = append(cands, pc)
	}
	intent := placement.Intent{Kind: placement.Auto, Home: l.Home, Owner: l.Owner, Exclude: l.Exclude}
	exact := true
	if l.V < 2 {
		// Version 1 kept no intent: an owner is known only when it was
		// followed, and an account the picker moved a session off not at all.
		if l.Reason == placement.ReasonOwner {
			intent.Owner = l.Chosen
		}
		exact = l.Mode == "auto" && l.Reason != placement.ReasonMoved
	}
	d := placement.Choose(cands, ledger, intent, now)
	out := Replay{Chosen: d.Chosen, Reason: d.Reason, Agrees: d.Chosen == l.Chosen, Exact: exact}
	for _, c := range cands {
		if c.Name != d.Chosen {
			continue
		}
		out.Chosen = e.display(c.Key)
		if !out.Agrees {
			lc, _ := find(l.Record, c.Name)
			out.After = e.after(c.Key, l.at, sessionCounted(lc))
		}
		break
	}
	return out
}
