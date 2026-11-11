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
	if d.Chosen != "other" || d.Reason != ReasonNearLimit || d.Model != "Fable" {
		t.Fatalf("a Fable launch chose %q (%s, model %q)", d.Chosen, d.Reason, d.Model)
	}
	if c, _ := d.Find("spent"); c.Excluded != "Fable (7d) 92%" || c.ModelRefusal != c.Excluded {
		t.Errorf("spent: excluded %q, model refusal %q", c.Excluded, c.ModelRefusal)
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

func TestNoAccountWithRoomOnAModelRefusesAndSaysWhenItRenews(t *testing.T) {
	late := withFable("late", 20, 95)
	soon := withFable("soon", 20, 91)
	soon.Limits[2].ResetAt = at(10 * time.Hour)
	out := withFable("out", 0, 5)
	out.Excluded = "login expired"
	d := chosen(t, []Candidate{late, soon, out}, Ledger{}, asksFable)
	want := "no account has room on Fable: late (Fable (7d) 95%), soon (Fable (7d) 91%); not eligible: out (login expired)"
	if d.Chosen != "" || d.Refusal != want {
		t.Fatalf("refusal = %q\nwant      %q", d.Refusal, want)
	}
	if d.RoomAt != at(10*time.Hour) {
		t.Errorf("room at %d, want the sooner renewal %d", d.RoomAt, at(10*time.Hour))
	}

	// The threshold itself refuses; one point under it admits.
	edge := withFable("edge", 0, ModelLimitPercent)
	if d := chosen(t, []Candidate{edge}, Ledger{}, asksFable); d.Chosen != "" {
		t.Fatalf("chose %q: a row at %d%% admits no launch for its model", d.Chosen, ModelLimitPercent)
	}
	edge.Limits[2].Percent = ModelLimitPercent - 1
	if d := chosen(t, []Candidate{edge}, Ledger{}, asksFable); d.Chosen != "edge" || d.Reason != ReasonNearLimit {
		t.Fatalf("chose %q (%s): %d%% is near, and still admits", d.Chosen, d.Reason, ModelLimitPercent-1)
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
}

func TestNamingAnAccountDoesNotGetPastAModelsLimit(t *testing.T) {
	spent := withFable("spent", 20, 97)
	spent.Limits[2].ResetAt = at(30 * time.Hour)
	room := withFable("room", 20, 50)
	cands := []Candidate{spent, room}
	named := func(account string, models ...string) Intent {
		return Intent{Kind: Forced, Account: account, Reason: "named", Models: models}
	}
	d := chosen(t, cands, Ledger{}, named("spent", "fable"))
	if d.Chosen != "" || d.Refusal != "spent cannot take a Fable launch: Fable (7d) 97%" || d.RoomAt != at(30*time.Hour) {
		t.Fatalf("refusal %q, room at %d", d.Refusal, d.RoomAt)
	}
	if d := chosen(t, cands, Ledger{}, named("room", "fable")); d.Chosen != "room" || d.Reason != "named" {
		t.Fatalf("a named account with room: %+v", d)
	}
	if d := chosen(t, cands, Ledger{}, named("spent")); d.Chosen != "spent" {
		t.Fatalf("a named launch asking for no model is the caller's: %+v", d)
	}
	// What else keeps an account out of automatic choices does not hide its
	// figure from a named launch.
	spent.Excluded = "not logged in"
	if d := chosen(t, []Candidate{spent, room}, Ledger{}, named("spent", "fable")); d.Chosen != "" || !strings.Contains(d.Refusal, "97%") {
		t.Fatalf("a named, otherwise excluded account: %+v", d)
	}
}

// A limit nothing measured on an account is not room for a launch it governs,
// though an ordinary launch still tries such an account.
func TestAnAccountWithNoFigureForTheModelCannotTakeItsLaunch(t *testing.T) {
	unmeasured := acct("unmeasured", 0, 10)
	never := Candidate{Name: "never", Key: "n"}
	measured := withFable("measured", 0, 40)
	measured.Busy = []Proc{{PID: 1, StartedMS: now.UnixMilli() - 3_600_000}}
	cands := []Candidate{unmeasured, never, measured}
	if d := chosen(t, cands, Ledger{}, Intent{}); d.Chosen == "measured" {
		t.Fatalf("an ordinary launch chose %q: the accounts with no Fable figure are idle, and tried", d.Chosen)
	}
	d := chosen(t, cands, Ledger{}, asksFable)
	if d.Chosen != "measured" {
		t.Fatalf("a Fable launch chose %q", d.Chosen)
	}
	for _, name := range []string{"unmeasured", "never"} {
		if c, _ := d.Find(name); c.Excluded != "no Fable figure" || c.ModelRow != nil {
			t.Errorf("%s: excluded %q, row %+v", name, c.Excluded, c.ModelRow)
		}
	}
	d = chosen(t, []Candidate{unmeasured, withFable("full", 0, 99)}, Ledger{}, asksFable)
	if d.Refusal != "no account has room on Fable: unmeasured (no Fable figure), full (Fable (7d) 99%)" || d.RoomAt != at(72*time.Hour) {
		t.Fatalf("refusal %q, room at %d", d.Refusal, d.RoomAt)
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
	// A percent that did not parse bounds nothing.
	bad := withFable("bad", 0, 0)
	bad.Limits[2].Bad = true
	if d := chosen(t, []Candidate{bad}, Ledger{}, asksFable); d.Chosen != "" || !strings.Contains(d.Refusal, "bad (Fable (7d) unreadable)") {
		t.Fatalf("an unparseable Fable figure: %+v", d)
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
