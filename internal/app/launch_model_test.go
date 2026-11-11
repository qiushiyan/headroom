package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// observeFable stores a reading with an all-models row and a Fable-scoped row,
// both renewing `left` from now, taken a minute ago.
func (f autoFixture) observeFable(name string, weekly, fable int, left time.Duration) {
	f.t.Helper()
	reset := time.Now().Add(left).UTC().Format(time.RFC3339)
	f.store(name, fmt.Sprintf(`{"limits":[`+
		`{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":%q},`+
		`{"kind":"weekly_scoped","group":"weekly","percent":%d,"resets_at":%q,"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}`,
		weekly, reset, fable, reset), time.Now().Add(-time.Minute))
}

// With every account near some limit, a bare launch takes the account whose
// highest figure is lowest. Asked for Fable, it never takes one with Fable
// spent: the arguments say so, and they reach the vendor unchanged.
func TestAFableLaunchNeverStartsWhereFableIsSpent(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observeFable("qiushi", 95, 10, 72*time.Hour)
	f.observeFable("a@x.com", 50, 92, 72*time.Hour)
	f.observeFable("b@x.com", 96, 20, 72*time.Hour)

	plain := f.launch("--dry-run")
	if !strings.Contains(plain.stdout, "would start claude on a@x.com") {
		t.Fatalf("an ordinary launch would take the lowest highest figure, a:\n%s", plain.stdout)
	}
	dry := f.launch("--dry-run", "--", "--model", "fable")
	for _, want := range []string{"would start claude on qiushi", "excluded: Fable (7d) 92%", "Fable (7d) 10%"} {
		if !strings.Contains(dry.stdout, want) {
			t.Errorf("Fable dry run lacks %q:\n%s", want, dry.stdout)
		}
	}

	got := f.launch("--", "--model", "fable", "-p", "review")
	if got.code != 0 || !got.called || got.account(f.cfg) != "qiushi" {
		t.Fatalf("Fable launch: %s", got)
	}
	if !slices.Equal(got.args, []string{"--model", "fable", "-p", "review"}) {
		t.Errorf("vendor args = %q", got.args)
	}
	if !strings.Contains(got.stderr, "Fable (7d) 10%") {
		t.Errorf("launch line lacks the Fable figure:\n%s", got.stderr)
	}
	recs := f.log()
	if len(recs) != 1 || recs[0].Model != "Fable" || recs[0].ModelPct == nil || *recs[0].ModelPct != 10 || recs[0].ModelBasis != "observed" {
		t.Errorf("log = %+v", recs)
	}
}

// With no account under the cutoff, a Fable launch starts nothing and its last
// line says when Fable renews and what to do meanwhile.
func TestAFableLaunchWithNoRoomRefusesAndStartsNothing(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.observeFable("qiushi", 40, 95, 72*time.Hour)
	f.observeFable("a@x.com", 30, 91, 20*time.Hour)

	dry := f.launch("--dry-run", "--", "--model=claude-fable-5-2")
	if dry.code != 1 || dry.called || !strings.Contains(dry.stdout, "would refuse — no account has room on Fable: qiushi (Fable (7d) 95%), a@x.com (Fable (7d) 91%)") {
		t.Fatalf("dry run: exit %d called %v:\n%s", dry.code, dry.called, dry.stdout)
	}

	got := f.launch("--", "--model", "fable")
	if got.code == 0 || got.called {
		t.Fatalf("a launch with no Fable room ran: %s", got)
	}
	lines := strings.Split(strings.TrimSpace(got.stderr), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(got.stderr, "no account has room on Fable") ||
		!strings.HasPrefix(last, "Fable has room again ") || !strings.Contains(last, "(in 20.0h)") ||
		!strings.HasSuffix(last, "run it on another model (--model opus), or wait until then") {
		t.Errorf("refusal:\n%s", got.stderr)
	}
	if recs, _, _ := readLaunchLog(f.cfg.AccountsRoot); len(recs) != 0 {
		t.Errorf("a refused launch was logged: %+v", recs)
	}
	if recent := f.st.Load().Placements().Recent; len(recent) != 0 {
		t.Errorf("a refused launch was recorded as load: %+v", recent)
	}

	// The same arguments without the model are an ordinary launch.
	if plain := f.launch("--", "-p", "hi"); plain.code != 0 || !plain.called {
		t.Fatalf("an ordinary launch: %s", plain)
	}
}

// Naming an account chooses where a launch runs, never whether its model has
// room there: a named Fable launch on a spent account refuses before anything
// is written, and one on an account with room says its figure.
func TestANamedFableLaunchOnASpentAccountRefusesAndRecordsNothing(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observeFable("qiushi", 20, 30, 72*time.Hour)
	f.observeFable("a@x.com", 40, 97, 30*time.Hour)
	f.observeFable("b@x.com", 40, 20, 72*time.Hour)
	// A placement long past its span: a refusal that reached the store would
	// prune it, and state.json would change.
	if _, err := f.st.Place(state.Launch{
		Candidates: []placement.Candidate{{Name: "b@x.com", Ref: "b@x.com", Key: "test:b"}},
		Intent:     placement.Intent{Kind: placement.Forced, Account: "b@x.com", Reason: "test"},
		Now:        time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(f.cfg.AccountsRoot, "state.json"), launchLogPath(f.cfg.AccountsRoot)}
	before := make([][]byte, len(files))
	for i, p := range files {
		before[i], _ = os.ReadFile(p)
	}

	got := f.launch("--account", "a@x.com", "--", "--model", "fable")
	if got.code == 0 || got.called {
		t.Fatalf("a named launch on a spent account ran: %s", got)
	}
	for _, want := range []string{"a@x.com cannot take a Fable launch: Fable (7d) 97%", "Fable has room again", "(in 1.2d)", "run it without --account, or on another model (--model opus), or wait until then"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("refusal lacks %q:\n%s", want, got.stderr)
		}
	}
	for i, p := range files {
		if after, _ := os.ReadFile(p); string(after) != string(before[i]) {
			t.Errorf("%s changed for a launch that refused", filepath.Base(p))
		}
	}

	ok := f.launch("--account", "b@x.com", "--", "--model", "fable")
	if ok.code != 0 || ok.account(f.cfg) != "b@x.com" || !strings.Contains(ok.stderr, "headroom launch: b@x.com · named · Fable (7d) 20%") {
		t.Fatalf("a named launch with room: %s", ok)
	}
	if recs := f.log(); len(recs) != 1 || recs[0].Mode != "named" || recs[0].Model != "Fable" || *recs[0].ModelPct != 20 {
		t.Errorf("log = %+v", recs)
	}
	// Every other account spent too: letting the rule choose is no way on, and
	// the refusal does not offer it.
	g := newAutoFixture(t, "a@x.com")
	g.observeFable("qiushi", 20, 95, 72*time.Hour)
	g.observeFable("a@x.com", 40, 97, 30*time.Hour)
	if alone := g.launch("--account", "a@x.com", "--", "--model", "fable"); alone.code == 0 || strings.Contains(alone.stderr, "without --account") ||
		!strings.Contains(alone.stderr, "run it on another model (--model opus), or wait until then") {
		t.Errorf("a named refusal with no other account free:\n%s", alone.stderr)
	}
	// Naming a spent account still works for a launch asking for no model.
	if plain := f.launch("--account", "a@x.com", "--", "-p", "hi"); plain.code != 0 || plain.account(f.cfg) != "a@x.com" {
		t.Fatalf("a named ordinary launch: %s", plain)
	}
}

// A Fable launch refused for another reason says that reason and nothing
// about Fable: run on Opus, it would be refused the same way.
func TestAFableLaunchRefusedForAnotherReasonSaysNothingOfFable(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.observeFable("qiushi", 10, 10, 72*time.Hour)
	f.observeFable("a@x.com", 10, 20, 72*time.Hour)
	past := time.Now().Add(-time.Hour).UnixMilli()
	f.edges.Creds = func(accounts.Account) string { // every login has ended
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"refreshTokenExpiresAt":%d}}`, past)
	}
	got := f.launch("--", "--model", "fable")
	if got.code == 0 || got.called || !strings.Contains(got.stderr, "no account can take a launch") ||
		!strings.Contains(got.stderr, "login expired") || strings.Contains(got.stderr, "room again") {
		t.Fatalf("refusal:\n%s", got.stderr)
	}
}

// A reading whose Fable row arrives twice says nothing usable about Fable on
// that account: the row still names the model, its figure is unreadable, and
// the account cannot take a Fable launch — across the usage parser, not just
// the rule.
func TestADamagedFableRowIsUnreadableNotAbsent(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	row := fmt.Sprintf(`{"kind":"weekly_scoped","group":"weekly","percent":5,"resets_at":%q,"scope":{"model":{"id":null,"display_name":"Fable"}}}`, reset)
	f.store("qiushi", fmt.Sprintf(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":10,"resets_at":%q},%s,%s]}`, reset, row, row), time.Now().Add(-time.Minute))
	f.observe("a@x.com", 0, 30, time.Minute) // no Fable row: a is tried
	dry := f.launch("--dry-run", "--", "--model", "fable")
	if !strings.Contains(dry.stdout, "would start claude on a@x.com") || !strings.Contains(dry.stdout, "excluded: Fable (7d) unreadable") {
		t.Fatalf("dry run:\n%s", dry.stdout)
	}
	if got := f.launch("--account", "qiushi", "--", "--model", "fable"); got.code == 0 || !strings.Contains(got.stderr, "qiushi cannot take a Fable launch: Fable (7d) unreadable") ||
		!strings.Contains(got.stderr, "nothing says when Fable has room again") {
		t.Fatalf("named launch on the damaged account:\n%s", got.stderr)
	}
}

// The log keeps what the chosen account's Fable figure rested on: an old
// reading is a lower bound, and says so.
func TestTheLogSaysWhatTheFableFigureRestsOn(t *testing.T) {
	f := newAutoFixture(t)
	reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	f.store("qiushi", fmt.Sprintf(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":10,"resets_at":%q},`+
		`{"kind":"weekly_scoped","group":"weekly","percent":33,"resets_at":%q,"scope":{"model":{"id":null,"display_name":"Fable"}}}]}`, reset, reset),
		time.Now().Add(-3*time.Hour))
	if got := f.launch("--", "--model", "fable"); got.code != 0 || !strings.Contains(got.stderr, "Fable (7d) ≥33%") {
		t.Fatalf("launch: %s", got)
	}
	if recs := f.log(); len(recs) != 1 || recs[0].ModelPct == nil || *recs[0].ModelPct != 33 || recs[0].ModelBasis != "stale" {
		t.Errorf("log = %+v", recs)
	}
}
