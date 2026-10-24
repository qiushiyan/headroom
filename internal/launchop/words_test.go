package launchop

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// The one line a launch prints, by the tokens a person and an agent read: the
// account and how it was decided, then — when the rule decided — each figure
// said as what it rests on. A figure the rule counted as a bound, or as an
// ended window, is said as one: "5h 0%" over a window that was at 97% a minute
// ago and has since reset is a measured-looking number nobody measured.
func TestLaunchLineWording(t *testing.T) {
	now := time.Unix(1_790_848_000, 0)
	limit := func(session bool, kind, label string, percent int, reset time.Duration) placement.Limit {
		return placement.Limit{Kind: kind, Label: label, Percent: percent, ResetAt: now.Add(reset).Unix(), Session: session}
	}
	rows := func(name string, age time.Duration, limits ...placement.Limit) placement.Candidate {
		return placement.Candidate{Name: name, Key: name, ObservedAt: now.Add(-age).Unix(), Limits: limits}
	}
	obs := func(name string, session, weekly int, age time.Duration) placement.Candidate {
		return rows(name, age, limit(true, "session", "5h session", session, time.Hour), limit(false, "weekly_all", "All models (7d)", weekly, 29*time.Hour))
	}
	choose := func(intent placement.Intent, cands ...placement.Candidate) placement.Decision {
		return placement.Choose(cands, placement.Ledger{}, intent, now)
	}
	cases := []struct {
		name  string
		d     placement.Decision
		auto  bool
		ref   Session
		owner string
		want  []string
		not   []string
	}{
		{"least load", choose(placement.Intent{}, obs("a", 12, 3, time.Minute), obs("b", 40, 3, time.Minute)), true, Session{}, "",
			[]string{"a · auto", "5h 12%", "week 3%", "resets in 1.2d", "load 1", "(next: b)"}, []string{"old"}},
		{"stale", choose(placement.Intent{}, obs("a", 0, 18, 40*time.Hour)), true, Session{}, "",
			[]string{"a · auto", "5h ≥0%", "week ≥18%", "figures 1d old", "load 0"}, []string{"next"}},
		{"never observed", choose(placement.Intent{}, placement.Candidate{Name: "a", Key: "a"}), true, Session{}, "",
			[]string{"a · auto", "never observed", "load 0"}, nil},
		{"an ended session window", choose(placement.Intent{}, rows("b", time.Minute,
			limit(true, "session", "5h session", 97, -30*time.Second), limit(false, "weekly_all", "All models (7d)", 12, 48*time.Hour))), true, Session{}, "",
			[]string{"5h window ended"}, []string{"5h 0%"}},
		// A weekly window that has ended is counted whole from now: there is
		// no reset instant to count down to, and none is printed.
		{"an ended week", choose(placement.Intent{}, rows("b", time.Minute,
			limit(true, "session", "5h session", 3, time.Hour), limit(false, "weekly_all", "All models (7d)", 64, -time.Minute))), true, Session{}, "",
			[]string{"week window ended"}, []string{"resets in"}},
		{"an unparseable percent", choose(placement.Intent{}, rows("b", time.Minute,
			placement.Limit{Kind: "session", Label: "5h session", Bad: true, Session: true})), true, Session{}, "",
			[]string{"5h ?%"}, []string{"100%"}},
		{"near a limit", choose(placement.Intent{}, obs("a", 10, 84, time.Minute)), true, Session{}, "",
			[]string{"a · auto", "every account is near a limit", "All models (7d) 84%", "resets in 1.2d"}, []string{"load"}},
		{"near a limit on old figures", choose(placement.Intent{}, obs("b", 10, 91, 40*time.Hour)), true, Session{}, "",
			[]string{"every account is near a limit", "≥91%", "figures 1d old"}, nil},
		{"owner", choose(placement.Intent{Owner: "a"}, obs("a", 30, 30, time.Minute), obs("b", 0, 0, time.Minute)), true, Session{Route: sessA}, "a",
			[]string{"a · auto", "this session's account"}, nil},
		{"moved", choose(placement.Intent{Owner: "a"}, obs("a", 30, 95, time.Minute), obs("b", 0, 0, time.Minute)), true, Session{Route: sessA}, "a",
			[]string{"b · auto", "session moved from a"}, nil},
		{"unowned", choose(placement.Intent{}, obs("a", 0, 0, time.Minute)), true, Session{Route: sessA, Record: sessA}, "",
			[]string{"session had no known account"}, nil},
		{"unidentified", choose(placement.Intent{}, obs("a", 0, 0, time.Minute)), true, Session{Unidentified: true}, "",
			[]string{"session not identified"}, nil},
		{"pinned", choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: "pinned"}, obs("a", 0, 0, time.Minute)), false, Session{}, "",
			[]string{"a · pinned", "turns auto on"}, []string{"auto ·", "load"}},
	}
	// The other ways of deciding say which they were, and no figures: the
	// rule chose nothing.
	for reason, want := range map[string]string{"named": "a · named", placement.ReasonLast: "a · the last account used", placement.ReasonOwner: "a · this session's account"} {
		cases = append(cases, struct {
			name  string
			d     placement.Decision
			auto  bool
			ref   Session
			owner string
			want  []string
			not   []string
		}{reason, choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: reason}, obs("a", 0, 0, time.Minute)), false, Session{}, "",
			[]string{want}, []string{"auto", "load"}})
	}
	for _, c := range cases {
		line := launchLine(c.d, c.auto, c.ref, c.owner, "", now)
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q lacks %q", c.name, line, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(line, n) {
				t.Errorf("%s: %q must not say %q", c.name, line, n)
			}
		}
	}
}
