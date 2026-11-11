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
	}
	// An account the rule could not consider for an expired login is counted
	// on the line of every automatic launch, with the command that renews it,
	// whether the rule had room or every account was near a limit. Any other
	// exclusion is not a login's to end.
	out := func(name, why string) placement.Candidate {
		c := obs(name, 0, 0, time.Minute)
		c.Excluded = why
		return c
	}
	cases = append(cases, []struct {
		name  string
		d     placement.Decision
		auto  bool
		ref   Session
		owner string
		want  []string
		not   []string
	}{
		{"one expired login", choose(placement.Intent{}, obs("a", 0, 0, time.Minute), obs("b", 5, 5, time.Minute), out("c", loginExpired)), true, Session{}, "",
			[]string{"a · auto", "load 0 · 1 account left out, login expired: run headroom login (next: b)"}, []string{"accounts left out"}},
		{"expired logins", choose(placement.Intent{}, obs("a", 0, 0, time.Minute), out("b", loginExpired), out("c", loginExpired)), true, Session{}, "",
			[]string{"a · auto", "2 accounts left out, login expired: run headroom login"}, nil},
		{"expired logins near a limit", choose(placement.Intent{}, obs("a", 10, 84, time.Minute), out("b", loginExpired)), true, Session{}, "",
			[]string{"every account is near a limit", "1 account left out, login expired: run headroom login"}, nil},
		{"a reservation is no login", choose(placement.Intent{}, obs("a", 0, 0, time.Minute), out("b", "reserved for ~/dev/x")), true, Session{}, "",
			[]string{"a · auto"}, []string{"left out"}},
		{"a named launch counts nothing", choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: "named"}, obs("a", 0, 0, time.Minute), out("b", loginExpired)), false, Session{}, "",
			[]string{"a · named"}, []string{"left out"}},
	}...)
	// A named launch says so, and no figures: the rule chose nothing.
	cases = append(cases, struct {
		name  string
		d     placement.Decision
		auto  bool
		ref   Session
		owner string
		want  []string
		not   []string
	}{"named", choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: "named"}, obs("a", 0, 0, time.Minute)), false, Session{}, "",
		[]string{"a · named"}, []string{"auto", "load"}})

	// A launch a model's own limit admitted prints the chosen account's figure
	// on it, said as what it rests on — named launches too, since that limit
	// admitted them as well.
	fable := func(name string, pct int, age time.Duration) placement.Candidate {
		c := obs(name, 0, 20, age)
		c.Limits = append(c.Limits, placement.Limit{Kind: "weekly_scoped", Label: "Fable (7d)", Percent: pct, ResetAt: now.Add(50 * time.Hour).Unix(), Model: "Fable"})
		return c
	}
	asks := []string{"fable"}
	for _, c := range []struct {
		name string
		d    placement.Decision
		auto bool
		want []string
	}{
		{"auto, Fable", choose(placement.Intent{Models: asks}, fable("a", 40, time.Minute)), true, []string{"a · auto", "load 0", "Fable (7d) 40%"}},
		{"auto, Fable from old figures", choose(placement.Intent{Models: asks}, fable("a", 40, 3*time.Hour)), true, []string{"Fable (7d) ≥40%"}},
		{"named, Fable", choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: "named", Models: asks}, fable("a", 70, time.Minute)), false, []string{"a · named · Fable (7d) 70%"}},
	} {
		if line := launchLine(c.d, c.auto, Session{}, "", now); !containsAll(line, c.want) {
			t.Errorf("%s: %q lacks %q", c.name, line, c.want)
		}
	}
	if line := launchLine(choose(placement.Intent{}, fable("a", 40, time.Minute)), true, Session{}, "", now); strings.Contains(line, "Fable") {
		t.Errorf("a launch asking for no model printed a model figure: %q", line)
	}
	for _, c := range cases {
		line := launchLine(c.d, c.auto, c.ref, c.owner, now)
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

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// A refusal by a model's own limit ends on a line of its own: when that limit
// renews and the ways on — the part a reader of a terminal's
// tail keeps. Any other refusal is said as the rule said it, a launch that
// asked for Fable included.
func TestRefusalWording(t *testing.T) {
	now := time.Unix(1_790_848_000, 0)
	plain := placement.Decision{Refusal: "no account can take a launch: a (login expired)", Model: "Fable"}
	if got := refusal(plain, now).Error(); got != plain.Refusal {
		t.Errorf("a refusal for another reason = %q", got)
	}
	d := placement.Decision{Refusal: "no account has room on Fable: a (Fable (7d) 95%)", Model: "Fable", HeldBy: "Fable", RoomAt: now.Add(30 * time.Hour).Unix()}
	got := strings.Split(refusal(d, now).Error(), "\n")
	if len(got) != 2 || got[0] != d.Refusal || !strings.HasPrefix(got[1], "Fable has room again ") ||
		!strings.Contains(got[1], " (in 1.2d)") ||
		!strings.HasSuffix(got[1], ": run it on another model (--model opus), or wait until then") {
		t.Errorf("refusal = %q", got)
	}
	d.Elsewhere = true
	if named := refusal(d, now).Error(); !strings.HasSuffix(named, ": run it without --account, or on another model (--model opus), or wait until then") {
		t.Errorf("named refusal with another account free = %q", named)
	}
	d.RoomAt = now.Add(3 * time.Hour).Unix()
	if soon := refusal(d, now).Error(); strings.Contains(soon, time.Unix(d.RoomAt, 0).Format("Mon ")) || !strings.Contains(soon, "(in 3.0h)") {
		t.Errorf("a renewal within the day = %q", soon)
	}
	d.RoomAt, d.Elsewhere = 0, false
	if unknown := refusal(d, now).Error(); !strings.HasSuffix(unknown, "\nnothing says when Fable has room again: run it on another model (--model opus), or wait") {
		t.Errorf("refusal with no renewal = %q", unknown)
	}
}
