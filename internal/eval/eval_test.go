package eval

import (
	"fmt"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/usage"
	"github.com/qiushiyan/headroom/internal/usagelog"
)

const home = "/h/.claude-accounts"

var t0 = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// limits is a session and a weekly row as the rule counts them.
func limits(session, weekly int, sessionReset, weeklyReset time.Time) []placement.Limit {
	return []placement.Limit{
		{Kind: "session", Label: "5h session", Percent: session, ResetAt: sessionReset.Unix(), Session: true},
		{Kind: "weekly_all", Label: "All models (7d)", Percent: weekly, ResetAt: weeklyReset.Unix(), Period: 7 * 24 * 3600},
	}
}

func cand(name string, observed time.Time, l []placement.Limit) placement.Candidate {
	return placement.Candidate{Name: name, Key: "uuid:" + name, ObservedAt: observed.Unix(), Source: "headroom_cache", Limits: l}
}

// line logs a launch the way placeLaunch does: the decision the rule made and
// what it was asked.
func line(when time.Time, mode string, intent placement.Intent, ledger placement.Ledger, cands ...placement.Candidate) Launch {
	d := placement.Choose(cands, ledger, intent, when)
	r := launchlog.New(d, intent, when)
	r.Vendor, r.Mode, r.Recorded = "claude", mode, true
	return Launch{Home: home, Record: r}
}

// logged is one usage-log line: a session and a weekly row.
func logged(when time.Time, name string, session, weekly int, sessionReset, weeklyReset time.Time) usagelog.Record {
	ok := usage.StateOK
	return usagelog.New(when, config.Claude, "uuid:"+name, name, home, []usage.Row{
		{Kind: "session", Label: "5h session", Percent: session, ResetAt: sessionReset.Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
		{Kind: "weekly_all", Label: "All models (7d)", Percent: weekly, ResetAt: weeklyReset.Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
		{Kind: "weekly_scoped", Model: "Fable", Label: "Fable (7d)", Percent: 0, ResetAt: weeklyReset.Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
	}, usage.Allowance{})
}

func build(launches []Launch, readings []usagelog.Record, now time.Time) Report {
	return Build(Input{Vendor: config.Claude, Home: home, Launches: launches, Usage: readings, Now: now})
}

func summary(t *testing.T, rep Report, rule string) RuleSummary {
	t.Helper()
	for _, s := range rep.Rules {
		if s.Rule == rule {
			return s
		}
	}
	t.Fatalf("no summary for %q in %+v", rule, rep.Rules)
	return RuleSummary{}
}

// Every input the rule reads must reach the launch line, or the replay of a
// line decided by this very rule disagrees with it. This is the guard on that:
// a new input that the line does not carry fails here, as does a rule that
// changed without a new name.
func TestTheReplayDecidesEveryLineAsItWasDecided(t *testing.T) {
	now := at(0)
	soon, week := at(2*time.Hour), at(3*24*time.Hour)
	ended := at(-time.Minute)
	cases := []struct {
		name   string
		intent placement.Intent
		ledger placement.Ledger
		cands  []placement.Candidate
	}{
		{"least load", placement.Intent{}, placement.Ledger{}, []placement.Candidate{
			cand("a", now, limits(40, 20, soon, week)), cand("b", now, limits(10, 60, soon, week)),
		}},
		{"busy sessions and pending launches", placement.Intent{}, placement.Ledger{Recent: []placement.Pending{
			{Key: "uuid:b", Name: "b", PID: 7, AtMS: at(-time.Minute).UnixMilli()},
			{Key: "uuid:b", Name: "b", PID: 8, AtMS: at(-2 * time.Minute).UnixMilli()},
		}}, []placement.Candidate{
			func() placement.Candidate {
				c := cand("a", now, limits(20, 20, soon, week))
				c.Busy = []placement.Proc{{PID: 1, StartedMS: 1}, {PID: 2, StartedMS: 1}}
				return c
			}(),
			cand("b", now, limits(0, 20, soon, week)),
			cand("c", now, limits(30, 20, soon, week)),
		}},
		{"a weekly room that lapses sooner breaks the tie", placement.Intent{}, placement.Ledger{}, []placement.Candidate{
			cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(0, 50, soon, at(20*time.Hour))),
		}},
		{"an ended window and stale figures", placement.Intent{}, placement.Ledger{}, []placement.Candidate{
			cand("a", at(-3*time.Hour), limits(70, 20, ended, week)), cand("b", now, limits(25, 20, soon, week)),
		}},
		{"near a limit everywhere", placement.Intent{}, placement.Ledger{}, []placement.Candidate{
			cand("a", now, limits(85, 20, soon, week)), cand("b", now, limits(20, 90, soon, week)),
		}},
		{"excluded and never observed", placement.Intent{}, placement.Ledger{}, []placement.Candidate{
			func() placement.Candidate {
				c := cand("a", now, limits(0, 0, soon, week))
				c.Excluded = "login expired"
				return c
			}(),
			{Name: "b", Key: "uuid:b"}, cand("c", now, limits(30, 20, soon, week)),
		}},
		{"ties go to the account placed longest ago", placement.Intent{}, placement.Ledger{Last: map[string]placement.Last{
			"uuid:a": {Name: "a", AtMS: at(-time.Hour).UnixMilli()}, "uuid:b": {Name: "b", AtMS: at(-2 * time.Hour).UnixMilli()},
		}}, []placement.Candidate{
			cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(0, 10, soon, week)),
		}},
		{"a session follows its owner", placement.Intent{Owner: "b"}, placement.Ledger{}, []placement.Candidate{
			cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(30, 10, soon, week)),
		}},
		{"the picker moves a session off its account", placement.Intent{Exclude: "a"}, placement.Ledger{}, []placement.Candidate{
			cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(30, 10, soon, week)), cand("c", now, limits(40, 10, soon, week)),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := line(now, "auto", c.intent, c.ledger, c.cands...)
			rep := build([]Launch{l}, nil, now.Add(time.Hour))
			if len(rep.Decisions) != 1 {
				t.Fatalf("decisions = %+v", rep.Decisions)
			}
			r := rep.Decisions[0].Replay
			if !r.Agrees || !r.Exact || r.Chosen != l.Chosen || r.Reason != l.Reason {
				t.Errorf("logged %s (%s), replayed %+v", l.Chosen, l.Reason, r)
			}
			if s := summary(t, rep, placement.Rule); !s.Replay.SameRule || s.Replay.Agreed != 1 {
				t.Errorf("replay stats = %+v", s.Replay)
			}
		})
	}
}

// A version 1 line carries no intent: the picker's moves cannot be rebuilt,
// and the replay says so rather than claiming agreement on a guessed input.
func TestAVersion1LineIsReplayedForWhatItSays(t *testing.T) {
	now := at(0)
	l := line(now, "picker", placement.Intent{Exclude: "a"}, placement.Ledger{},
		cand("a", now, limits(0, 10, at(time.Hour), at(48*time.Hour))), cand("b", now, limits(30, 10, at(time.Hour), at(48*time.Hour))))
	l.V, l.Automatic, l.Exclude = 1, false, ""
	for i := range l.Candidates {
		l.Candidates[i].Key = ""
	}
	rep := Build(Input{Vendor: config.Claude, Home: home, Launches: []Launch{l}, Now: now.Add(time.Hour),
		Keys: map[string]string{home + "\x00a": "uuid:a", home + "\x00b": "uuid:b"}})
	if len(rep.Decisions) != 1 {
		t.Fatal("a version 1 picker line whose reason only the rule gives was not read as automatic")
	}
	if r := rep.Decisions[0].Replay; r.Exact || r.Agrees {
		t.Errorf("replay = %+v: the excluded account is unknown, so it cannot agree exactly", r)
	}
	if rep.Sources.Unkeyed != 0 {
		t.Errorf("the caller's keys were not used: %d unkeyed", rep.Sources.Unkeyed)
	}
	// A forced picker line — the session's own account — is no decision.
	forced := line(now, "picker", placement.Intent{Kind: placement.Forced, Account: "a", Reason: placement.ReasonOwner}, placement.Ledger{},
		cand("a", now, nil))
	forced.V = 1
	if rep := build([]Launch{forced}, nil, now); len(rep.Decisions) != 0 {
		t.Errorf("a forced picker line was read as the rule's: %+v", rep.Decisions)
	}
}

// A window filled by pinned launches is not the rule's doing: it counts under
// Pinned, with the launches that went there and the room another account had.
// The same window with an automatic launch in it is the rule's.
func TestAWindowIsChargedToWhoeverPlacedWorkThere(t *testing.T) {
	sessionReset, week := at(5*time.Hour), at(4*24*time.Hour)
	var usage []usagelog.Record
	for i, pct := range []int{5, 30, 60, 85, 92} {
		usage = append(usage, logged(at(time.Duration(i)*30*time.Minute), "a", pct, 20, sessionReset, week))
	}
	usage = append(usage, logged(at(80*time.Minute), "b", 10, 20, at(3*time.Hour), week))

	pinned := func(when time.Time) Launch {
		return line(when, "pinned", placement.Intent{Kind: placement.Forced, Account: "a", Reason: "pinned"}, placement.Ledger{},
			cand("a", when, limits(5, 20, sessionReset, week)), cand("b", when, limits(10, 20, at(3*time.Hour), week)))
	}
	launches := []Launch{pinned(at(-time.Minute)), pinned(at(10 * time.Minute)), pinned(at(40 * time.Minute))}
	rep := build(launches, usage, at(6*time.Hour))
	if len(rep.Episodes) != 1 {
		t.Fatalf("episodes = %+v", rep.Episodes)
	}
	ep := rep.Episodes[0]
	if ep.Placed || ep.Account != "a" || ep.Peak != 92 || ep.Launches["pinned"] != 3 || ep.NearAt != stamp(at(90*time.Minute).Unix()) {
		t.Errorf("episode = %+v", ep)
	}
	if ep.Elsewhere == nil || ep.Elsewhere.Account != "b" || ep.Elsewhere.Session != 10 {
		t.Errorf("room elsewhere = %+v", ep.Elsewhere)
	}
	if rep.Pinned.Session.ReachedNear != 1 || rep.Pinned.Session.RoomElsewhere != 1 || summary(t, rep, placement.Rule).Session.ReachedNear != 0 {
		t.Errorf("charged to the rule: pinned %+v", rep.Pinned.Session)
	}
	if rep.Pinned.Time.NearS == 0 || rep.Pinned.Time.SqueezedS == 0 || summary(t, rep, placement.Rule).Time.NearS != 0 {
		t.Errorf("near time: pinned %+v, rule %+v", rep.Pinned.Time, summary(t, rep, placement.Rule).Time)
	}

	auto := line(at(20*time.Minute), "auto", placement.Intent{}, placement.Ledger{},
		cand("a", at(20*time.Minute), limits(5, 20, sessionReset, week)), cand("b", at(20*time.Minute), limits(40, 20, at(3*time.Hour), week)))
	if auto.Chosen != "a" {
		t.Fatalf("fixture: the rule chose %s", auto.Chosen)
	}
	rep = build(append(launches, auto), usage, at(6*time.Hour))
	if ep := rep.Episodes[0]; !ep.Placed || ep.Rule != placement.Rule || ep.Launches["auto"] != 1 {
		t.Errorf("with an automatic launch in it: %+v", ep)
	}
	if s := summary(t, rep, placement.Rule); s.Session.ReachedNear != 1 || s.Time.NearS == 0 {
		t.Errorf("the rule's column = %+v", s)
	}
	// The launch is followed through its window: decided on 5%, peaked at 92%.
	if d := rep.Decisions[0]; d.ChosenAfter == nil || d.ChosenAfter.Counted != 5 || d.ChosenAfter.Peak != 92 {
		t.Errorf("after = %+v", d.ChosenAfter)
	}
	if s := summary(t, rep, placement.Rule); s.Outcomes.Followed != 1 || s.Outcomes.ReachedNear != 1 || s.Outcomes.MeanRise != 87 {
		t.Errorf("outcomes = %+v", s.Outcomes)
	}
}

// A renewal is the account's: the limit read highest at that instant is what
// bound it, and a scoped limit at zero beside it lapsed nothing. Weeks the
// schedule implies with no reading are unobserved, never lapsed.
func TestARenewalCountsTheBindingLimit(t *testing.T) {
	r1 := at(24 * time.Hour)
	r4 := r1.Add(21 * 24 * time.Hour)
	usage := []usagelog.Record{
		logged(at(0), "a", 10, 30, at(time.Hour), r1),
		logged(at(20*time.Hour), "a", 10, 45, at(21*time.Hour), r1),
		logged(r4.Add(-time.Hour), "a", 10, 5, r4.Add(-30*time.Minute), r4),
		logged(at(10*time.Hour), "b", 10, 85, at(11*time.Hour), at(5*24*time.Hour)),
	}
	l := line(at(-time.Hour), "auto", placement.Intent{}, placement.Ledger{}, cand("a", at(-time.Hour), limits(0, 30, at(time.Hour), r1)))
	rep := build([]Launch{l}, usage, r4.Add(time.Hour))
	var got []Renewal
	for _, r := range rep.Renewals {
		if r.Account == "a" {
			got = append(got, r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("renewals of a = %+v", got)
	}
	first := got[0]
	// b's weekly limit reached 85% inside a's window: room lapsed on a while
	// another account was squeezed.
	if first.Limit != "All models (7d)" || first.Used != 45 || first.Lapsed != 55 || first.LastReadS != 4*3600 || first.PeakElsewhere != 85 {
		t.Errorf("first renewal = %+v", first)
	}
	s := summary(t, rep, placement.Rule)
	if s.Weekly.Unobserved != 2 {
		t.Errorf("unobserved renewals = %d, want the 2 weeks between", s.Weekly.Unobserved)
	}
	// a's first week had an automatic launch in it; its last week and b's
	// had none.
	if s.Weekly.Renewals != 1 || s.Weekly.LapsedSqueeze != 1 || rep.Pinned.Weekly.Renewals != 2 {
		t.Errorf("the placed week counts under the rule, the unplaced ones under pinned: rule %+v, pinned %+v", s.Weekly, rep.Pinned.Weekly)
	}
}

// Launch lines carry the figures each account was read at. Without a usage
// log those are the readings; with one, the same reading is counted once.
func TestFiguresCarriedInLaunchLinesAreReadingsToo(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	var launches []Launch
	for i := range 3 {
		when := at(time.Duration(i) * 10 * time.Minute)
		launches = append(launches, line(when, "auto", placement.Intent{}, placement.Ledger{},
			cand("a", at(-time.Minute), limits(20, 20, at(time.Hour), week)), cand("b", when, limits(10*i, 20, at(time.Hour), week))))
	}
	rep := build(launches, nil, at(time.Hour))
	if rep.Sources.Carried != 4 || rep.Sources.Logged != 0 {
		t.Errorf("carried %d, logged %d: a's one reading and b's three", rep.Sources.Carried, rep.Sources.Logged)
	}
	inLog := []usagelog.Record{logged(at(-time.Minute), "a", 20, 20, at(time.Hour), week)}
	rep = build(launches, inLog, at(time.Hour))
	if rep.Sources.Carried != 3 || rep.Sources.Logged != 1 {
		t.Errorf("carried %d, logged %d: the logged reading is not carried again", rep.Sources.Carried, rep.Sources.Logged)
	}
}

// Time is attributed to the rule of the home the report is made from; a
// decision to its own line's rule, whichever home made it.
func TestAnotherHomesOlderRuleKeepsItsDecisionsNotTheTime(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	mine := line(at(0), "auto", placement.Intent{}, placement.Ledger{}, cand("a", at(0), limits(10, 10, at(time.Hour), week)))
	theirs := line(at(time.Minute), "auto", placement.Intent{}, placement.Ledger{}, cand("a", at(time.Minute), limits(10, 10, at(time.Hour), week)))
	theirs.Home, theirs.Rule = "/other/.claude-accounts", "load-0"
	usage := []usagelog.Record{logged(at(30*time.Minute), "a", 12, 10, at(time.Hour), week)}
	rep := build([]Launch{mine, theirs}, usage, at(time.Hour))
	old := summary(t, rep, "load-0")
	if old.Launches.Automatic != 1 || old.Time.ObservedS != 0 || old.Replay.SameRule {
		t.Errorf("another home's rule = %+v", old)
	}
	if s := summary(t, rep, placement.Rule); s.Time.ObservedS == 0 || s.Launches.Automatic != 1 {
		t.Errorf("this home's rule = %+v", s)
	}
	if rep.Decisions[1].Home != "/other/.claude-accounts" {
		t.Errorf("decision home = %q", rep.Decisions[1].Home)
	}
}

// --since narrows what is counted, never what is known: a window that reached
// its limit before it is not an episode, and readings before it still say
// where an account stood.
func TestSinceNarrowsTheCount(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	var launches []Launch
	for i := range 4 {
		when := at(time.Duration(i) * time.Hour)
		launches = append(launches, line(when, "auto", placement.Intent{}, placement.Ledger{}, cand("a", when, limits(10*i, 10, at(5*time.Hour), week))))
	}
	rep := Build(Input{Vendor: config.Claude, Home: home, Launches: launches, Now: at(5 * time.Hour), Since: at(150 * time.Minute)})
	if s := summary(t, rep, placement.Rule); s.Launches.Total != 1 || len(rep.Decisions) != 1 {
		t.Errorf("since: %d launches, %d decisions", s.Launches.Total, len(rep.Decisions))
	}
	if rep.Since == nil || *rep.Since != stamp(at(150*time.Minute).Unix()) {
		t.Errorf("since = %v", rep.Since)
	}
}

func TestNothingRecordedIsAnEmptyReport(t *testing.T) {
	rep := build(nil, nil, t0)
	if len(rep.Rules) != 0 || rep.Sources.First != nil || rep.Episodes == nil || rep.Decisions == nil {
		t.Errorf("empty report = %+v", rep)
	}
	_ = fmt.Sprint(rep)
}
