package eval

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/usage"
	"github.com/qiushiyan/headroom/internal/usagelog"
)

var update = flag.Bool("update", false, "freeze the replay scenarios as testdata/<rule>.jsonl for the rule this binary decides by")

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
// everything it was made from.
func line(when time.Time, mode string, intent placement.Intent, ledger placement.Ledger, cands ...placement.Candidate) Launch {
	d := placement.Choose(cands, ledger, intent, when)
	r := launchlog.New(d, cands, ledger, intent, when)
	r.Vendor, r.Mode, r.Recorded = "claude", mode, true
	return Launch{Home: home, Record: r}
}

// auto is an automatic launch on a set of candidates and nothing else.
func auto(when time.Time, cands ...placement.Candidate) Launch {
	return line(when, "auto", placement.Intent{}, placement.Ledger{}, cands...)
}

// pinned is a launch pinned to an account.
func pinned(when time.Time, name string, cands ...placement.Candidate) Launch {
	return line(when, "pinned", placement.Intent{Kind: placement.Forced, Account: name, Reason: "pinned"}, placement.Ledger{}, cands...)
}

// logged is one usage-log line: a session and a weekly row, and a scoped
// weekly row at zero beside them.
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

// scenarios are decisions whose every input matters to the choice.
func scenarios() []struct {
	name   string
	intent placement.Intent
	ledger placement.Ledger
	cands  []placement.Candidate
} {
	now := at(0)
	soon, week := at(2*time.Hour), at(3*24*time.Hour)
	ended := at(-time.Minute)
	busy := func(c placement.Candidate, procs ...placement.Proc) placement.Candidate { c.Busy = procs; return c }
	excluded := func(c placement.Candidate, why string) placement.Candidate { c.Excluded = why; return c }
	alias := func(c placement.Candidate, name string) placement.Candidate { c.Name = name; return c }
	return []struct {
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
			busy(cand("a", now, limits(20, 20, soon, week)), placement.Proc{PID: 1, StartedMS: 1}, placement.Proc{PID: 2, StartedMS: 1}),
			cand("b", now, limits(0, 20, soon, week)),
			cand("c", now, limits(30, 20, soon, week)),
		}},
		{"a launch that became a busy session counts once", placement.Intent{}, placement.Ledger{Recent: []placement.Pending{
			{Key: "uuid:a", Name: "a", PID: 41, AtMS: at(-time.Minute).UnixMilli()},
		}}, []placement.Candidate{
			busy(cand("a", now, limits(0, 20, soon, week)), placement.Proc{PID: 41, StartedMS: at(-time.Minute).UnixMilli()}),
			cand("b", now, limits(10, 20, soon, week)),
		}},
		{"two dirs on one subscription count its launches once", placement.Intent{}, placement.Ledger{Recent: []placement.Pending{
			{Key: "uuid:a", Name: "a", PID: 5, AtMS: at(-time.Minute).UnixMilli()},
		}}, []placement.Candidate{
			cand("a", now, limits(10, 10, soon, week)), alias(cand("a", now, limits(10, 10, soon, week)), "a2"),
			cand("b", now, limits(20, 10, soon, week)),
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
			excluded(cand("a", now, limits(0, 0, soon, week)), "login expired"),
			{Name: "b", Key: "uuid:b"}, cand("c", now, limits(30, 20, soon, week)),
		}},
		{"ties go to the account placed longest ago, to the millisecond", placement.Intent{}, placement.Ledger{Last: map[string]placement.Last{
			"uuid:a": {Name: "a", AtMS: at(-time.Hour).UnixMilli() + 900}, "uuid:b": {Name: "b", AtMS: at(-time.Hour).UnixMilli() + 100},
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
}

// Every input the rule reads reaches the launch line: a line decided by this
// rule is decided alike when replayed, input for input. A new input the line
// does not carry fails here.
func TestEveryInputTheRuleReadsReachesTheLine(t *testing.T) {
	for _, c := range scenarios() {
		t.Run(c.name, func(t *testing.T) {
			l := line(at(0), "auto", c.intent, c.ledger, c.cands...)
			rep := build([]Launch{l}, nil, at(time.Hour))
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

// Lines frozen when the rule was named are decided alike while it keeps the
// name: a rule whose choices change has a new name, so a line in the log is
// never credited to a rule that would not have made it. A new rule freezes
// its own lines: go test ./internal/eval -run TestFrozen -update.
func TestFrozenLinesOfThisRuleAreDecidedAlike(t *testing.T) {
	path := filepath.Join("testdata", placement.Rule+".jsonl")
	if *update {
		var b bytes.Buffer
		for _, c := range scenarios() {
			l := line(at(0), "auto", c.intent, c.ledger, c.cands...)
			out, _ := json.Marshal(l.Record)
			b.Write(append(out, '\n'))
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no frozen lines for rule %s (%v): freeze them with -update once its choices are final", placement.Rule, err)
	}
	var launches []Launch
	for _, raw := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r launchlog.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		launches = append(launches, Launch{Home: home, Record: r})
	}
	rep := build(launches, nil, at(time.Hour))
	if len(rep.Decisions) != len(launches) || len(launches) < 5 {
		t.Fatalf("%d decisions from %d frozen lines", len(rep.Decisions), len(launches))
	}
	for _, d := range rep.Decisions {
		switch {
		case !d.Replay.Exact:
			t.Errorf("a frozen line of %s no longer carries its whole input: the record lost a field the replay reads", placement.Rule)
		case !d.Replay.Agrees:
			t.Errorf("rule %s no longer decides a line it made (%s chose %s, now %s): give the changed rule a new name",
				placement.Rule, d.At, d.Chosen, d.Replay.Chosen)
		}
	}
}

// A fault is a disagreement no difference between rules explains: a line the
// replayed rule itself decided, whole input on record, now decided otherwise
// — here, a rule whose choice changed under its old name. Another rule's
// disagreement is the replay working, and an inexact line's is its missing
// input.
func TestAFaultIsADisagreementNoRuleDifferenceExplains(t *testing.T) {
	now := at(0)
	soon, week := at(2*time.Hour), at(3*24*time.Hour)
	decided := func() Launch {
		return auto(now, cand("a", now, limits(10, 10, soon, week)), cand("b", now, limits(40, 10, soon, week)))
	}
	changed := decided()
	changed.Chosen = "b" // what a rule renamed in nothing but its code would have logged
	older := decided()
	older.Chosen, older.Rule = "b", "load-0"
	v1 := line(now, "picker", placement.Intent{Exclude: "a"}, placement.Ledger{},
		cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(30, 10, soon, week)))
	v1.V, v1.Automatic, v1.Exclude = 1, false, ""

	rep := build([]Launch{decided(), changed, older, v1}, nil, now.Add(time.Hour))
	var faults []bool
	for _, d := range rep.Decisions {
		faults = append(faults, d.Replay.Fault)
	}
	if want := []bool{false, true, false, false}; fmt.Sprint(faults) != fmt.Sprint(want) {
		t.Errorf("faults = %v, want %v", faults, want)
	}
	if s := summary(t, rep, placement.Rule); s.Replay.Faults != 1 || s.Replay.Replayed != 3 {
		t.Errorf("this rule's replay = %+v", s.Replay)
	}
	if s := summary(t, rep, "load-0"); s.Replay.Faults != 0 || s.Replay.Agreed != 0 {
		t.Errorf("an older rule's replay = %+v", s.Replay)
	}
}

// A version 1 line carries counts, not processes, and no intent: it is
// replayed from what it carries, never credited with an exactness it lacks.
func TestAnOlderLineIsReplayedForWhatItCarries(t *testing.T) {
	now := at(0)
	soon, week := at(time.Hour), at(48*time.Hour)
	v1 := func(l Launch) Launch {
		l.V, l.Automatic, l.Exclude, l.Owner, l.Recent = 1, false, "", "", nil
		for i := range l.Candidates {
			l.Candidates[i].Key, l.Candidates[i].BusyProcs = "", nil
		}
		return l
	}
	keys := map[string]string{home + "\x00a": "uuid:a", home + "\x00a2": "uuid:a", home + "\x00b": "uuid:b", home + "\x00c": "uuid:c"}
	replay := func(l Launch) Replay {
		t.Helper()
		rep := Build(Input{Vendor: config.Claude, Home: home, Launches: []Launch{l}, Now: now.Add(time.Hour), Keys: keys})
		if len(rep.Decisions) != 1 {
			t.Fatalf("decisions = %+v", rep.Decisions)
		}
		if rep.Sources.Unkeyed != 0 {
			t.Errorf("the caller's keys were not used: %d unkeyed", rep.Sources.Unkeyed)
		}
		return rep.Decisions[0].Replay
	}

	picker := v1(line(now, "picker", placement.Intent{Exclude: "a"}, placement.Ledger{},
		cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(30, 10, soon, week))))
	if r := replay(picker); r.Exact || r.Agrees {
		t.Errorf("a picker move without its excluded account: %+v", r)
	}
	plain := v1(auto(now, cand("a", now, limits(10, 10, soon, week)), cand("b", now, limits(30, 10, soon, week))))
	if r := replay(plain); !r.Exact || !r.Agrees {
		t.Errorf("a line with nothing but figures is whole: %+v", r)
	}
	// Two dirs on one subscription each counted its one launch: it goes back
	// once, and the counts make the line inexact for any other counting.
	shared := cand("a", now, limits(10, 10, soon, week))
	twin := shared
	twin.Name = "a2"
	aliases := v1(line(now, "auto", placement.Intent{}, placement.Ledger{Recent: []placement.Pending{{Key: "uuid:a", Name: "a", PID: 5, AtMS: now.Add(-time.Minute).UnixMilli()}}},
		shared, twin, cand("b", now, limits(20, 10, soon, week))))
	if r := replay(aliases); !r.Agrees || r.Exact {
		t.Errorf("aliases' launch: %+v", r)
	}
	// A tie inside one second the line cannot order.
	tie := auto(now, cand("a", now, limits(0, 10, soon, week)), cand("b", now, limits(0, 10, soon, week)))
	for i, ms := range []int64{900, 100} {
		s := at(-time.Hour).Add(time.Duration(ms) * time.Millisecond).UTC().Format(time.RFC3339)
		tie.Candidates[i].LastPlaced = &s
	}
	if r := replay(v1(tie)); r.Exact {
		t.Errorf("a same-second tie read as exact: %+v", r)
	}

	forced := line(now, "picker", placement.Intent{Kind: placement.Forced, Account: "a", Reason: placement.ReasonOwner}, placement.Ledger{},
		cand("a", now, nil))
	forced.V = 1
	if rep := build([]Launch{forced}, nil, now); len(rep.Decisions) != 0 {
		t.Errorf("a forced picker line was read as the rule's: %+v", rep.Decisions)
	}
}

// A window counts under the rule whose automatic launch went into it, and as
// Unplaced when only pinned launches did — with how long it sat near its
// limit and how much of that another account had room.
func TestAWindowCountsUnderWhoeverPlacedWorkThere(t *testing.T) {
	sessionReset, week := at(5*time.Hour), at(4*24*time.Hour)
	var usage []usagelog.Record
	for i, pct := range []int{5, 30, 60, 85, 92} {
		usage = append(usage, logged(at(time.Duration(i)*30*time.Minute), "a", pct, 20, sessionReset, week))
	}
	// b is read every half hour from 80m — fresh, with room, until 200m — and
	// its window ends at 3h: room on that basis to the end.
	for i, pct := range []int{10, 11, 12, 13} {
		usage = append(usage, logged(at(80*time.Minute+time.Duration(i)*30*time.Minute), "b", pct, 20, at(3*time.Hour), week))
	}

	pin := func(when time.Time) Launch {
		return pinned(when, "a", cand("a", when, limits(5, 20, sessionReset, week)), cand("b", when, limits(10, 20, at(3*time.Hour), week)))
	}
	launches := []Launch{pin(at(-time.Minute)), pin(at(10 * time.Minute)), pin(at(40 * time.Minute))}
	rep := build(launches, usage, at(6*time.Hour))
	if len(rep.Episodes) != 1 {
		t.Fatalf("episodes = %+v", rep.Episodes)
	}
	ep := rep.Episodes[0]
	if ep.Placed || ep.Rule != "" || ep.Account != "a" || ep.Peak != 92 || ep.Launches["pinned"] != 3 || ep.NearAt != stamp(at(90*time.Minute).Unix()) {
		t.Errorf("episode = %+v", ep)
	}
	// Near from 90m to the reset at 5h, and b had room all along.
	if ep.NearS != int64((5*time.Hour-90*time.Minute)/time.Second) || ep.SqueezedS != ep.NearS {
		t.Errorf("near %ds, squeezed %ds", ep.NearS, ep.SqueezedS)
	}
	if ep.Elsewhere == nil || ep.Elsewhere.Account != "b" || ep.Elsewhere.Session != 10 || ep.Elsewhere.Basis != "fresh" {
		t.Errorf("room elsewhere = %+v", ep.Elsewhere)
	}
	if u := rep.Unplaced.Session; u.ReachedNear != 1 || u.RoomElsewhere != 1 || u.NearS != ep.NearS || summary(t, rep, placement.Rule).Session.ReachedNear != 0 {
		t.Errorf("unplaced %+v", u)
	}

	// An automatic launch before the window's first reading went into it too.
	first := auto(at(-20*time.Minute), cand("a", at(-20*time.Minute), nil), cand("b", at(-20*time.Minute), limits(40, 20, at(3*time.Hour), week)))
	if first.Chosen != "a" {
		t.Fatalf("fixture: the rule chose %s", first.Chosen)
	}
	rep = build(append([]Launch{first}, launches...), usage, at(6*time.Hour))
	if ep := rep.Episodes[0]; !ep.Placed || ep.Rule != placement.Rule || ep.Launches["auto"] != 1 || ep.Launches["pinned"] != 3 {
		t.Errorf("with an automatic launch in it: %+v", ep)
	}
	if s := summary(t, rep, placement.Rule); s.Session.ReachedNear != 1 || s.Session.NearS == 0 || rep.Unplaced.Session.ReachedNear != 0 {
		t.Errorf("the rule's column = %+v, unplaced %+v", s.Session, rep.Unplaced.Session)
	}
	if d := rep.Decisions[0]; d.ChosenAfter == nil || d.ChosenAfter.Counted != 0 || d.ChosenAfter.Peak != 92 {
		t.Errorf("after = %+v: the window opened after the launch and went to 92", d.ChosenAfter)
	}
}

// A launch is followed through the window it went into: decided on 60% just
// before a reset, its work went into the window open then, and a reading of
// the next window says nothing about it.
func TestALaunchIsFollowedThroughItsOwnWindow(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	l := auto(at(0), cand("a", at(0), limits(60, 10, at(10*time.Minute), week)))
	rep := build([]Launch{l}, []usagelog.Record{
		logged(at(5*time.Minute), "a", 64, 10, at(10*time.Minute), week),
		logged(at(30*time.Minute), "a", 10, 10, at(5*time.Hour), week),
	}, at(time.Hour))
	if a := rep.Decisions[0].ChosenAfter; a == nil || a.Counted != 60 || a.Peak != 64 || a.ResetsAt != stamp(at(10*time.Minute).Unix()) {
		t.Errorf("after = %+v", a)
	}
	if s := summary(t, rep, placement.Rule); s.Outcomes.MeanRise != 4 {
		t.Errorf("rise = %v", s.Outcomes.MeanRise)
	}
	// With nothing read of the window it went into, the launch is not
	// followed — a later window's figure is no stand-in.
	rep = build([]Launch{l}, []usagelog.Record{logged(at(30*time.Minute), "a", 10, 10, at(5*time.Hour), week)}, at(time.Hour))
	if a := rep.Decisions[0].ChosenAfter; a != nil {
		t.Errorf("followed into another window: %+v", a)
	}
	// A window whose reset is further from the launch than Follow did not
	// open from it.
	far := auto(at(0), cand("a", at(0), nil))
	rep = build([]Launch{far}, []usagelog.Record{logged(at(time.Hour), "a", 30, 10, at(7*time.Hour), week)}, at(8*time.Hour))
	if a := rep.Decisions[0].ChosenAfter; a != nil {
		t.Errorf("a window resetting %v after the launch claimed it: %+v", 7*time.Hour, a)
	}
}

// Room is room on evidence: a fresh reading, or a session window that has
// ended since an older one. An old reading of a window still open is a lower
// bound and shows no room.
func TestRoomElsewhereNeedsEvidence(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	near := []usagelog.Record{logged(at(0), "a", 50, 10, at(4*time.Hour), week), logged(at(time.Hour), "a", 85, 10, at(4*time.Hour), week)}
	for _, c := range []struct {
		name  string
		b     usagelog.Record
		basis string
	}{
		{"fresh", logged(at(50*time.Minute), "b", 30, 10, at(3*time.Hour), week), "fresh"},
		{"an old reading of a window that has ended", logged(at(-10*time.Hour), "b", 70, 10, at(-8*time.Hour), week), "ended"},
		{"an old reading of a window still open", logged(at(-16*time.Hour), "b", 30, 10, at(10*time.Hour), week), ""},
		{"fresh but too full", logged(at(50*time.Minute), "b", 60, 10, at(3*time.Hour), week), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := build(nil, append([]usagelog.Record{c.b}, near...), at(2*time.Hour))
			if len(rep.Episodes) != 1 {
				t.Fatalf("episodes = %+v", rep.Episodes)
			}
			got := rep.Episodes[0].Elsewhere
			switch {
			case c.basis == "" && got != nil:
				t.Errorf("room = %+v", got)
			case c.basis != "" && (got == nil || got.Basis != c.basis):
				t.Errorf("room = %+v, want basis %q", got, c.basis)
			}
		})
	}
}

// A renewal is the account's: the limit read highest at that instant is what
// bound it, and a scoped limit at zero beside it lapsed nothing. Its figure is
// a lower bound and the room an upper one. Weeks the schedule implies with no
// reading are unobserved, never lapsed.
func TestARenewalCountsTheBindingLimit(t *testing.T) {
	r1 := at(24 * time.Hour)
	r4 := r1.Add(21 * 24 * time.Hour)
	usage := []usagelog.Record{
		logged(at(0), "a", 10, 30, at(time.Hour), r1),
		logged(at(20*time.Hour), "a", 10, 45, at(21*time.Hour), r1),
		logged(r4.Add(-time.Hour), "a", 10, 5, r4.Add(-30*time.Minute), r4),
		logged(at(10*time.Hour), "b", 10, 85, at(11*time.Hour), at(5*24*time.Hour)),
	}
	l := auto(at(-time.Hour), cand("a", at(-time.Hour), limits(0, 30, at(time.Hour), r1)))
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
	// b's weekly limit reached 85% inside a's window: room may have lapsed
	// on a while another account was squeezed.
	first := got[0]
	if first.Limit != "All models (7d)" || first.UsedMin != 45 || first.LapsedMax != 55 || first.LastReadS != 4*3600 || first.PeakElsewhere != 85 || !first.Placed {
		t.Errorf("first renewal = %+v", first)
	}
	if rep.Fleet.UnobservedRenewals != 2 {
		t.Errorf("unobserved renewals = %d, want the 2 weeks between", rep.Fleet.UnobservedRenewals)
	}
	// a's first week had an automatic launch in it; its last week and b's
	// had none.
	s := summary(t, rep, placement.Rule)
	if s.Weekly.Renewals != 1 || s.Weekly.LapsedMaxPts != 55 || s.Weekly.MeanUsedMin != 45 || s.Weekly.LapsedWhileSqueezed != 1 {
		t.Errorf("the rule's weeks = %+v", s.Weekly)
	}
	if u := rep.Unplaced.Weekly; u.Renewals != 2 || u.LapsedMaxPts != 95+15 || u.MeanUsedMin != 45 {
		t.Errorf("unplaced weeks = %+v", u)
	}
}

// Launch lines carry the figures each account was read at. Without a usage
// log those are the readings; with one, the same reading is counted once.
func TestFiguresCarriedInLaunchLinesAreReadingsToo(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	var launches []Launch
	for i := range 3 {
		when := at(time.Duration(i) * 10 * time.Minute)
		launches = append(launches, auto(when, cand("a", at(-time.Minute), limits(20, 20, at(time.Hour), week)), cand("b", when, limits(10*i, 20, at(time.Hour), week))))
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

// A decision counts under its own line's rule, whichever home made it, and so
// does a window its launch went into.
func TestAnotherHomesOlderRuleKeepsItsOwnDecisionsAndWindows(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	theirs := auto(at(0), cand("a", at(0), limits(10, 10, at(time.Hour), week)))
	theirs.Home, theirs.Rule = "/other/.claude-accounts", "load-0"
	mine := auto(at(time.Minute), cand("b", at(time.Minute), limits(10, 10, at(time.Hour), week)))
	usage := []usagelog.Record{logged(at(30*time.Minute), "a", 90, 10, at(time.Hour), week)}
	rep := build([]Launch{theirs, mine}, usage, at(2*time.Hour))
	old := summary(t, rep, "load-0")
	if old.Launches.Automatic != 1 || old.Session.ReachedNear != 1 || old.Replay.SameRule {
		t.Errorf("another home's rule = %+v", old)
	}
	if s := summary(t, rep, placement.Rule); s.Session.ReachedNear != 0 || s.Launches.Automatic != 1 {
		t.Errorf("this home's rule = %+v", s)
	}
	if rep.Decisions[0].Home != "/other/.claude-accounts" || rep.Episodes[0].Rule != "load-0" {
		t.Errorf("decision %+v, episode %+v", rep.Decisions[0], rep.Episodes[0])
	}
}

// --since narrows what is counted, never what is known: launches before it,
// windows that reached their limit before it and renewals before it are not
// counted, and readings before it still say where an account stood.
func TestSinceNarrowsTheCount(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	var launches []Launch
	for i := range 4 {
		when := at(time.Duration(i) * time.Hour)
		launches = append(launches, auto(when, cand("a", when, limits(10*i, 10, at(5*time.Hour), week))))
	}
	usage := []usagelog.Record{
		logged(at(30*time.Minute), "b", 85, 50, at(2*time.Hour), at(time.Hour)), // near, and a renewal, before since
		logged(at(4*time.Hour), "a", 90, 10, at(5*time.Hour), week),             // near after it
	}
	since := at(150 * time.Minute)
	rep := Build(Input{Vendor: config.Claude, Home: home, Launches: launches, Usage: usage, Now: at(6 * time.Hour), Since: since})
	if s := summary(t, rep, placement.Rule); s.Launches.Total != 1 || len(rep.Decisions) != 1 {
		t.Errorf("since: %d launches, %d decisions", s.Launches.Total, len(rep.Decisions))
	}
	if len(rep.Episodes) != 1 || rep.Episodes[0].Account != "a" || len(rep.Renewals) != 0 {
		t.Errorf("episodes %+v, renewals %+v", rep.Episodes, rep.Renewals)
	}
	if rep.Since == nil || *rep.Since != stamp(since.Unix()) || rep.Fleet.ObservedS > int64((6*time.Hour-150*time.Minute)/time.Second) {
		t.Errorf("since = %v, fleet %+v", rep.Since, rep.Fleet)
	}
}

// The vendor's instant drifts by fractions of a second between readings; a
// window is the same window to the minute.
func TestAResetThatDriftsBySecondsIsOneWindow(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	reset := at(3 * time.Hour)
	rep := build(nil, []usagelog.Record{
		logged(at(0), "a", 70, 10, reset.Add(-time.Second), week),
		logged(at(time.Hour), "a", 85, 10, reset, week),
	}, at(2*time.Hour))
	if len(rep.Episodes) != 1 || rep.Episodes[0].Peak != 85 || rep.Episodes[0].NearAt != stamp(at(time.Hour).Unix()) {
		t.Errorf("episodes = %+v", rep.Episodes)
	}
}

// The summary's figures are counts and times a reader can redo by hand.
func TestTheFiguresAddUp(t *testing.T) {
	week := at(4 * 24 * time.Hour)
	soon := at(5 * time.Hour)
	a := func(when time.Time, age time.Duration, session int) placement.Candidate {
		return cand("a", when.Add(-age), limits(session, 10, soon, week))
	}
	b := func(when time.Time, age time.Duration, session int) placement.Candidate {
		return cand("b", when.Add(-age), limits(session, 10, soon, week))
	}
	launches := []Launch{
		auto(at(0), a(at(0), time.Minute, 0), b(at(0), time.Minute, 10)), // a, figures 1m old
		auto(at(10*time.Minute), a(at(10*time.Minute), 2*time.Minute, 0), b(at(10*time.Minute), 3*time.Minute, 10)),
		auto(at(20*time.Minute), a(at(20*time.Minute), 30*time.Minute, 50), b(at(20*time.Minute), 3*time.Minute, 10)), // b
		pinned(at(30*time.Minute), "a", a(at(30*time.Minute), time.Minute, 50), b(at(30*time.Minute), time.Minute, 10)),
	}
	for i, want := range []string{"a", "a", "b", "a"} {
		if launches[i].Chosen != want {
			t.Fatalf("fixture: launch %d chose %s", i, launches[i].Chosen)
		}
	}
	rep := build(launches, nil, at(time.Hour))
	l := summary(t, rep, placement.Rule).Launches
	if l.Total != 4 || l.Automatic != 3 || l.ByMode["auto"] != 3 || l.ByMode["pinned"] != 1 || l.ByReason[placement.ReasonLeastLoad] != 3 ||
		l.ByAccount["a"] != 2 || l.ByAccount["b"] != 1 || l.LongestRun != 2 {
		t.Errorf("launches = %+v", l)
	}
	// The chosen figures were 60s, 120s and 180s old.
	if l.ChosenAgeP50S != 120 || l.ChosenAgeP90S != 180 || l.ChosenStale != 0 {
		t.Errorf("ages: p50 %d p90 %d stale %d", l.ChosenAgeP50S, l.ChosenAgeP90S, l.ChosenStale)
	}

	// The fleet: a near from 30m to the 1h end, b fresh at 10% beside it.
	usage := []usagelog.Record{
		logged(at(0), "a", 40, 10, soon, week), logged(at(0), "b", 10, 10, soon, week),
		logged(at(30*time.Minute), "a", 80, 10, soon, week), logged(at(30*time.Minute), "b", 20, 10, soon, week),
	}
	f := build(nil, usage, at(time.Hour)).Fleet
	// The last readings are fresh until 60m, the report's end: an hour
	// observed, a near from 30m on, b with room throughout.
	if f.ObservedS != 3600 || f.NearS != 1800 || f.SqueezedS != 1800 {
		t.Errorf("fleet = %+v", f)
	}
	// Spread: 30 points over the first half hour, 60 over the second.
	if f.MeanSpreadPts != 45 {
		t.Errorf("spread = %v, want 45", f.MeanSpreadPts)
	}
	// One reading is fresh for FreshFor, and that is all it observes.
	if f := build(nil, usage[:1], at(time.Hour)).Fleet; f.ObservedS != int64(FreshFor/time.Second) {
		t.Errorf("one reading observed %ds", f.ObservedS)
	}
}

// Codex is read by its own rules: the shortest window of the main limit is
// the session window, a feature's limit decides nothing, and a blocked account
// has no room.
func TestCodexIsReadByItsOwnRules(t *testing.T) {
	ok := usage.StateOK
	codex := func(when time.Time, name string, session, weekly, review int, allowance usage.AllowanceState) usagelog.Record {
		return usagelog.New(when, config.Codex, "uuid:"+name, name, home, []usage.Row{
			{Kind: "primary", Group: usage.CodexGroupMain, WindowSeconds: 18000, Label: "5h", Percent: session, ResetAt: at(3 * time.Hour).Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
			{Kind: "secondary", Group: usage.CodexGroupMain, WindowSeconds: 604800, Label: "7d", Percent: weekly, ResetAt: at(72 * time.Hour).Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
			{Kind: "primary", Group: usage.CodexGroupCodeReview, WindowSeconds: 18000, Label: "5h code review", Percent: review, ResetAt: at(3 * time.Hour).Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
		}, usage.Allowance{State: allowance})
	}
	readings := []usagelog.Record{
		codex(at(0), "x", 40, 10, 100, usage.AllowanceAllowed),
		codex(at(time.Hour), "x", 85, 10, 100, usage.AllowanceAllowed),
		codex(at(50*time.Minute), "y", 5, 10, 0, usage.AllowanceBlocked),
	}
	rep := Build(Input{Vendor: config.Codex, Home: home, Usage: readings, Now: at(2 * time.Hour)})
	if len(rep.Episodes) != 1 {
		t.Fatalf("episodes = %+v: a spent code review limit decides nothing", rep.Episodes)
	}
	ep := rep.Episodes[0]
	if !ep.Session || ep.Limit != "5h" || ep.Peak != 85 || ep.Elsewhere != nil {
		t.Errorf("episode = %+v: the 5h main window is the session; a blocked account has no room", ep)
	}
	readings[2] = codex(at(50*time.Minute), "y", 5, 10, 0, usage.AllowanceAllowed)
	if ep := Build(Input{Vendor: config.Codex, Home: home, Usage: readings, Now: at(2 * time.Hour)}).Episodes[0]; ep.Elsewhere == nil || ep.Elsewhere.Account != "y" {
		t.Errorf("an allowed account's room = %+v", ep.Elsewhere)
	}
}

// A window that starts with a request opens a moment after the launch that
// made it: the launch went into it, and is followed through it — for Follow
// and no further, however long the window runs.
func TestALaunchOpensTheWindowThatStartsAfterIt(t *testing.T) {
	ok := usage.StateOK
	codex := func(when time.Time, pct int, windowS int64, reset time.Time) usagelog.Record {
		return usagelog.New(when, config.Codex, "uuid:x", "x", home, []usage.Row{
			{Kind: "primary", Group: usage.CodexGroupMain, WindowSeconds: windowS, Label: "w", Percent: pct, ResetAt: reset.Unix(), PercentState: ok, ResetState: ok, IdentityState: ok},
		}, usage.Allowance{})
	}
	l := line(at(0), "auto", placement.Intent{}, placement.Ledger{}, placement.Candidate{Name: "x", Key: "uuid:x"})
	l.Vendor = "codex"
	opened := at(2 * time.Minute)
	rep := Build(Input{Vendor: config.Codex, Home: home, Launches: []Launch{l}, Now: at(6 * time.Hour), Usage: []usagelog.Record{
		codex(at(time.Hour), 40, 18000, opened.Add(5*time.Hour)), codex(at(3*time.Hour), 85, 18000, opened.Add(5*time.Hour)),
	}})
	if d := rep.Decisions[0]; d.ChosenAfter == nil || d.ChosenAfter.Peak != 85 || d.ChosenAfter.Counted != 0 {
		t.Errorf("after = %+v", d.ChosenAfter)
	}
	if len(rep.Episodes) != 1 || !rep.Episodes[0].Placed || rep.Episodes[0].Launches["auto"] != 1 {
		t.Errorf("episodes = %+v", rep.Episodes)
	}
	// A week-long window, as Codex's shortest main window can be: followed
	// for Follow, its later readings say nothing about this launch.
	week := opened.Add(7 * 24 * time.Hour)
	rep = Build(Input{Vendor: config.Codex, Home: home, Launches: []Launch{l}, Now: at(12 * time.Hour), Usage: []usagelog.Record{
		codex(at(time.Hour), 20, 604800, week), codex(at(10*time.Hour), 60, 604800, week),
	}})
	if a := rep.Decisions[0].ChosenAfter; a == nil || a.Peak != 20 {
		t.Errorf("after = %+v: a reading %v after the launch is past Follow", a, 10*time.Hour)
	}
}

// The rule is replayed at the instant it decided, to the millisecond: a
// recent launch on the edge of PendingFor counts or not as it did then.
func TestTheReplayKeepsTheDecisionsMillisecond(t *testing.T) {
	now := at(0).Add(700 * time.Millisecond)
	soon, week := at(2*time.Hour), at(3*24*time.Hour)
	// 900.4s old at the decision: no longer pending. At the second the line
	// used to keep, it would be 900.0s old — pending still.
	edge := now.Add(-placement.PendingFor - 400*time.Millisecond)
	ledger := placement.Ledger{Recent: []placement.Pending{{Key: "uuid:a", Name: "a", PID: 9, AtMS: edge.UnixMilli()}}}
	l := line(now, "auto", placement.Intent{}, ledger, cand("a", now, limits(10, 10, soon, week)), cand("b", now, limits(10, 50, soon, week)))
	rep := build([]Launch{l}, nil, now.Add(time.Hour))
	if r := rep.Decisions[0].Replay; !r.Agrees || !r.Exact {
		t.Errorf("logged %s, replayed %+v", l.Chosen, r)
	}
}

func TestNothingRecordedIsAnEmptyReport(t *testing.T) {
	rep := Build(Input{Vendor: config.Claude, Home: home, Now: t0, Problems: []string{"cannot read x"}})
	if len(rep.Rules) != 0 || rep.Sources.First != nil || rep.Episodes == nil || rep.Decisions == nil || len(rep.Sources.Problems) != 1 {
		t.Errorf("empty report = %+v", rep)
	}
}
