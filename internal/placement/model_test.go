package placement

import (
	"strings"
	"testing"
	"time"
)

// withFable is acct with a Fable-scoped weekly row beside its all-models one,
// resetting in three days.
func withFable(name string, weekly, fable int) Candidate {
	c := acct(name, 0, weekly)
	c.Limits = append(c.Limits, Limit{Kind: "weekly_scoped", Label: "Fable (7d)", Percent: fable, ResetAt: at(72 * time.Hour), Model: "Fable"})
	return c
}

// asksFable is an automatic launch whose arguments say --model fable.
var asksFable = Intent{Models: []string{"fable"}}

// The case this exists for: with every account near some limit, an ordinary
// launch takes the one whose highest figure is lowest — here an account with
// 8% of its Fable week left. A launch asking for Fable never does.
func TestALaunchAskingForAModelNeverStartsWhereItsLimitIsSpent(t *testing.T) {
	spent := withFable("spent", 50, 92)
	other := withFable("other", 95, 10)
	if d := chosen(t, []Candidate{spent, other}, Ledger{}, Intent{}); d.Chosen != "spent" || d.Model != "" {
		t.Fatalf("an ordinary launch chose %q (model %q): the lowest highest figure goes first", d.Chosen, d.Model)
	}
	d := chosen(t, []Candidate{spent, other}, Ledger{}, asksFable)
	if d.Chosen != "other" || d.Reason != ReasonNearLimit || d.Model != "Fable" || d.HeldBy != "" {
		t.Fatalf("a Fable launch chose %q (%s, model %q, held by %q)", d.Chosen, d.Reason, d.Model, d.HeldBy)
	}
	if c, _ := d.Find("spent"); c.Excluded != "Fable (7d) 92%" || c.ModelRefusal != c.Excluded || !c.HeldBack {
		t.Errorf("spent: excluded %q, model refusal %q, held back %v", c.Excluded, c.ModelRefusal, c.HeldBack)
	}
	if c, _ := d.Find("other"); c.ModelRow == nil || c.ModelRow.Counted != 10 || c.ModelRefusal != "" {
		t.Errorf("other: model row %+v, refusal %q", c.ModelRow, c.ModelRefusal)
	}
}

// Admission adds no preference: where a launch may run, it is ranked exactly
// as one that asks for nothing, so load still decides before Fable room.
func TestAModelsLimitAdmitsAndNeverRanks(t *testing.T) {
	busy := withFable("busy", 30, 10)
	busy.Busy = []Proc{{PID: 1, StartedMS: now.UnixMilli() - 3_600_000}}
	quiet := withFable("quiet", 30, 85)
	plain := chosen(t, []Candidate{busy, quiet}, Ledger{}, Intent{})
	asked := chosen(t, []Candidate{busy, quiet}, Ledger{}, asksFable)
	if plain.Chosen != "busy" || asked.Chosen != plain.Chosen || asked.RunnerUp != plain.RunnerUp {
		t.Fatalf("ordinary chose %q, next %q; Fable chose %q, next %q", plain.Chosen, plain.RunnerUp, asked.Chosen, asked.RunnerUp)
	}

	// Launches started together still spread: each pending launch is load,
	// and a forty-point difference in Fable room moves nothing.
	cands := []Candidate{withFable("a", 0, 40), withFable("b", 0, 0)}
	var ledger Ledger
	var got []string
	for i := range 4 {
		d := chosen(t, cands, ledger, asksFable)
		got = append(got, d.Chosen)
		ledger.Record("", "uuid:"+d.Chosen, d.Chosen, 100+i, now)
	}
	if strings.Join(got, ",") != "b,a,b,a" {
		t.Fatalf("four Fable launches went to %v, want them spread", got)
	}
}

// The owner set the cutoff at 90%: a figure of 90 refuses, 89 admits. The
// figures are written out, not derived from the constant, so moving the
// constant fails here.
func TestTheCutoffIsNinetyPercent(t *testing.T) {
	if d := chosen(t, []Candidate{withFable("edge", 0, 90)}, Ledger{}, asksFable); d.Chosen != "" || d.HeldBy != "Fable" {
		t.Fatalf("chose %q: a Fable figure of 90%% admits no Fable launch", d.Chosen)
	}
	if d := chosen(t, []Candidate{withFable("edge", 0, 89)}, Ledger{}, asksFable); d.Chosen != "edge" || d.Reason != ReasonNearLimit {
		t.Fatalf("chose %q (%s): 89%% is near a limit, and still admits", d.Chosen, d.Reason)
	}
}

func TestNoAccountWithRoomOnAModelRefusesAndSaysWhenItRenews(t *testing.T) {
	late := withFable("late", 20, 95)
	soon := withFable("soon", 20, 91)
	soon.Limits[2].ResetAt = at(10 * time.Hour)
	// Logged out, and its Fable renews sooner still: it is not an account the
	// launch could take, so its renewal is not when room returns.
	out := withFable("out", 0, 99)
	out.Limits[2].ResetAt = at(time.Hour)
	out.Excluded = "login expired"
	d := chosen(t, []Candidate{late, soon, out}, Ledger{}, asksFable)
	want := "no account has room on Fable: late (Fable (7d) 95%), soon (Fable (7d) 91%); not eligible: out (login expired)"
	if d.Chosen != "" || d.Refusal != want || d.HeldBy != "Fable" {
		t.Fatalf("refusal = %q (held by %q)\nwant      %q", d.Refusal, d.HeldBy, want)
	}
	if d.RoomAt != at(10*time.Hour) {
		t.Errorf("room at %d, want the eligible account's renewal %d", d.RoomAt, at(10*time.Hour))
	}
	if c, _ := d.Find("out"); c.HeldBack || c.ModelRefusal == "" {
		t.Errorf("out: held back %v, model refusal %q", c.HeldBack, c.ModelRefusal)
	}

	// An old reading is a lower bound and says so; a renewal nothing dates
	// leaves RoomAt unknown.
	stale := withFable("stale", 0, 93)
	stale.ObservedAt = at(-10 * time.Hour)
	stale.Limits[2].ResetAt = 0
	d = chosen(t, []Candidate{stale}, Ledger{}, asksFable)
	if !strings.Contains(d.Refusal, "stale (Fable (7d) ≥93%)") || d.RoomAt != 0 {
		t.Fatalf("refusal %q, room at %d", d.Refusal, d.RoomAt)
	}

	// A percent that did not parse bounds nothing, before its reset or after:
	// its reset is no promise of room.
	bad := withFable("bad", 0, 0)
	bad.Limits[2].Bad = true
	bad.Limits[2].ResetAt = at(2 * time.Hour)
	d = chosen(t, []Candidate{bad}, Ledger{}, asksFable)
	if d.Chosen != "" || !strings.Contains(d.Refusal, "bad (Fable (7d) unreadable)") || d.RoomAt != 0 {
		t.Fatalf("an unparseable Fable figure: refusal %q, room at %d", d.Refusal, d.RoomAt)
	}
	if later := Choose([]Candidate{bad}, Ledger{}, asksFable, now.Add(3*time.Hour)); later.Chosen != "" {
		t.Fatalf("past its reset an unparseable figure admitted the launch: %+v", later)
	}
}

// A Fable launch refused for another reason is that refusal, said as the rule
// says it: a model is no way out of an expired login.
func TestAModelLaunchRefusedForAnotherReasonIsNotHeldByTheModel(t *testing.T) {
	a, b := withFable("a", 0, 10), withFable("b", 0, 20)
	a.Excluded, b.Excluded = "login expired", "login expired"
	d := chosen(t, []Candidate{a, b}, Ledger{}, asksFable)
	if d.Chosen != "" || d.HeldBy != "" || d.Model != "Fable" || !strings.HasPrefix(d.Refusal, "no account can take a launch:") {
		t.Fatalf("held by %q, model %q, refusal %q", d.HeldBy, d.Model, d.Refusal)
	}
	gone := Choose([]Candidate{a}, Ledger{}, Intent{Kind: Forced, Account: "nobody", Models: []string{"fable"}}, now)
	if gone.HeldBy != "" || gone.Chosen != "" {
		t.Fatalf("a named account that does not exist: %+v", gone)
	}
}

func TestNamingAnAccountDoesNotGetPastAModelsLimit(t *testing.T) {
	spent := withFable("spent", 20, 97)
	spent.Limits[2].ResetAt = at(30 * time.Hour)
	room := withFable("room", 20, 50)
	named := func(account string, models ...string) Intent {
		return Intent{Kind: Forced, Account: account, Reason: "named", Models: models}
	}
	d := chosen(t, []Candidate{spent, room}, Ledger{}, named("spent", "fable"))
	if d.Chosen != "" || d.Refusal != "spent cannot take a Fable launch: Fable (7d) 97%" || d.HeldBy != "Fable" || d.RoomAt != at(30*time.Hour) {
		t.Fatalf("refusal %q, held by %q, room at %d", d.Refusal, d.HeldBy, d.RoomAt)
	}
	if !d.Elsewhere {
		t.Error("room could take it automatically, and the refusal must say so")
	}
	if d := chosen(t, []Candidate{spent, withFable("full", 20, 95)}, Ledger{}, named("spent", "fable")); d.Chosen != "" || d.Elsewhere {
		t.Fatalf("with every other account spent, letting the rule choose is no way on: %+v", d)
	}
	if d := chosen(t, []Candidate{spent, room}, Ledger{}, named("room", "fable")); d.Chosen != "room" || d.Reason != "named" {
		t.Fatalf("a named account with room: %+v", d)
	}
	if d := chosen(t, []Candidate{spent, room}, Ledger{}, named("spent")); d.Chosen != "spent" {
		t.Fatalf("a named launch asking for no model is the caller's: %+v", d)
	}
	// What else keeps an account out of automatic choices does not hide its
	// figure from a named launch.
	spent.Excluded = "not logged in"
	if d := chosen(t, []Candidate{spent, room}, Ledger{}, named("spent", "fable")); d.Chosen != "" || d.HeldBy != "Fable" || !strings.Contains(d.Refusal, "97%") {
		t.Fatalf("a named, otherwise excluded account: %+v", d)
	}
}

// An account whose reading has no figure for the model is tried, as any
// account nothing has measured is: what the cutoff stops is a launch onto a
// figure known to be spent.
func TestAnAccountWithNoFigureForTheModelIsTried(t *testing.T) {
	unmeasured := acct("unmeasured", 0, 10)
	full := withFable("full", 0, 99)
	d := chosen(t, []Candidate{full, unmeasured}, Ledger{}, asksFable)
	if d.Chosen != "unmeasured" {
		t.Fatalf("chose %q", d.Chosen)
	}
	if c, _ := d.Find("unmeasured"); c.Excluded != "" || c.ModelRow != nil || c.ModelRefusal != "" {
		t.Errorf("unmeasured: excluded %q, row %+v, refusal %q", c.Excluded, c.ModelRow, c.ModelRefusal)
	}
	if d := chosen(t, []Candidate{full, {Name: "never", Key: "n"}}, Ledger{}, asksFable); d.Chosen != "never" {
		t.Fatalf("chose %q: an account never observed is tried", d.Chosen)
	}
	// Readings of different ages: an old one that still carries an ended
	// Fable row does not make a fresh one without the row a refusal.
	old := withFable("old", 0, 40)
	old.ObservedAt, old.Limits[2].ResetAt, old.Excluded = at(-8*24*time.Hour), at(-24*time.Hour), "login expired"
	if d := chosen(t, []Candidate{old, acct("fresh", 0, 10)}, Ledger{}, asksFable); d.Chosen != "fresh" {
		t.Fatalf("chose %q: refusal %q", d.Chosen, d.Refusal)
	}
}

// Whether a model has a limit of its own is read from the readings: a launch
// asking for a model no reading limits — Opus today, or Fable the day the
// vendor drops its row — is placed like any other.
func TestAModelNoReadingLimitsIsAnOrdinaryLaunch(t *testing.T) {
	cands := []Candidate{acct("a", 0, 60), acct("b", 0, 10)}
	plain := chosen(t, cands, Ledger{}, Intent{})
	asked := chosen(t, cands, Ledger{}, asksFable)
	if asked.Chosen != plain.Chosen || asked.Model != "" {
		t.Fatalf("chose %q (model %q), an ordinary launch %q", asked.Chosen, asked.Model, plain.Chosen)
	}
	// Claude Code's catalog resolves "best" to Opus; "default" names no model.
	limited := []Candidate{withFable("a", 0, 95), withFable("b", 85, 10)}
	for _, models := range [][]string{{"opus"}, {"default"}, {"best"}, {""}, {"--settings"}} {
		if d := chosen(t, limited, Ledger{}, Intent{Models: models}); d.Model != "" || d.Chosen != "b" {
			t.Errorf("--model %q: model %q, chose %q", models, d.Model, d.Chosen)
		}
	}
	// A row whose period has ended counts as zero for its model too.
	ended := withFable("ended", 0, 99)
	ended.Limits[2].ResetAt = at(-time.Hour)
	if d := chosen(t, []Candidate{ended}, Ledger{}, asksFable); d.Chosen != "ended" || d.Model != "Fable" {
		t.Fatalf("an ended Fable window: %+v", d)
	}
}

// An account with several rows for the model is judged by its tightest,
// whichever order the reading lists them in.
func TestTheTightestRowForTheModelDecides(t *testing.T) {
	for _, order := range [][2]int{{85, 92}, {92, 85}} {
		c := acct("a", 0, 10)
		c.Limits = append(c.Limits,
			Limit{Kind: "weekly_scoped", Label: "Fable (7d)", Percent: order[0], ResetAt: at(72 * time.Hour), Model: "Fable"},
			Limit{Kind: "session_scoped", Label: "Fable (5h)", Percent: order[1], ResetAt: at(2 * time.Hour), Model: "Fable"})
		d := chosen(t, []Candidate{c}, Ledger{}, asksFable)
		if d.Chosen != "" || !strings.Contains(d.Refusal, "92%") {
			t.Errorf("rows %v: chose %q, refusal %q", order, d.Chosen, d.Refusal)
		}
	}
}

func TestModelNamesMeetByFamily(t *testing.T) {
	limited := []Candidate{withFable("a", 0, 95), withFable("b", 85, 10)}
	for _, asked := range []string{"fable", "Fable", "FABLE", "claude-fable-5-2", "claude-fable-5-1", "fable[1m]", " fable "} {
		if d := chosen(t, limited, Ledger{}, Intent{Models: []string{asked}}); d.Model != "Fable" || d.Chosen != "b" {
			t.Errorf("--model %q: model %q, chose %q", asked, d.Model, d.Chosen)
		}
	}
	// The vendor's own name may carry the family's prefix and a version.
	for _, row := range []string{"Claude Fable 5", "Fable 5", "claude-fable"} {
		c := withFable("a", 0, 95)
		c.Limits[2].Model = row
		if d := chosen(t, []Candidate{c}, Ledger{}, asksFable); d.Model != row || d.Chosen != "" {
			t.Errorf("row %q: model %q, chose %q", row, d.Model, d.Chosen)
		}
	}
	// The first model asked for that some reading limits is the one admitted.
	if d := chosen(t, limited, Ledger{}, Intent{Models: []string{"opus", "fable"}}); d.Model != "Fable" {
		t.Errorf("--model opus --model fable: model %q", d.Model)
	}
	for _, name := range []string{"opusplan", "sonnet[1m]", "claude-opus-5-5"} {
		if got := family(name); strings.Contains(got, "fable") || got == "" {
			t.Errorf("family(%q) = %q", name, got)
		}
	}
}
