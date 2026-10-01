package placement

import (
	"strings"
	"testing"
	"time"
)

var now = time.Unix(1_790_848_000, 0)

func at(d time.Duration) int64 { return now.Add(d).Unix() }

// acct builds a candidate observed just now: a session row and one weekly row,
// both resetting in the future.
func acct(name string, session, weekly int) Candidate {
	return Candidate{
		Name: name, Key: "uuid:" + name, ObservedAt: at(-time.Minute),
		Limits: []Limit{
			{Kind: "session", Label: "5h session", Percent: session, ResetAt: at(2 * time.Hour), Session: true},
			{Kind: "weekly_all", Label: "All models (7d)", Percent: weekly, ResetAt: at(72 * time.Hour)},
		},
	}
}

func chosen(t *testing.T, cands []Candidate, ledger Ledger, intent Intent) Decision {
	t.Helper()
	d := Choose(cands, ledger, intent, now)
	if d.Rule != Rule {
		t.Fatalf("rule = %q", d.Rule)
	}
	return d
}

func TestRowsCountOnlyForTheWindowTheyDescribe(t *testing.T) {
	c := Candidate{
		Name: "a", Key: "k", ObservedAt: at(-40 * time.Hour),
		Limits: []Limit{
			{Kind: "session", Percent: 70, ResetAt: at(-36 * time.Hour), Session: true},
			{Kind: "weekly_all", Percent: 18, ResetAt: at(24 * time.Hour)},
			{Kind: "weekly_scoped", Percent: 4},
		},
	}
	got := chosen(t, []Candidate{c}, Ledger{}, Intent{}).Candidates[0]
	if !got.Stale {
		t.Error("a 40h-old observation is stale")
	}
	want := []struct {
		counted int
		basis   Basis
	}{{0, BasisEnded}, {18, BasisStale}, {4, BasisStale}}
	for i, w := range want {
		if got.Limits[i].Counted != w.counted || got.Limits[i].Basis != w.basis {
			t.Errorf("row %d = %d/%s, want %d/%s", i, got.Limits[i].Counted, got.Limits[i].Basis, w.counted, w.basis)
		}
	}
	if got.Load != 0 || got.Weekly != 18 || got.NearLimit {
		t.Errorf("load %d weekly %d near %v", got.Load, got.Weekly, got.NearLimit)
	}

	fresh := acct("b", 34, 10)
	g := chosen(t, []Candidate{fresh}, Ledger{}, Intent{}).Candidates[0]
	if g.Stale || g.Limits[0].Basis != BasisObserved || g.Load != 3 {
		t.Errorf("fresh: stale %v basis %s load %d", g.Stale, g.Limits[0].Basis, g.Load)
	}
}

func TestAnUnparseablePercentSetsTheAccountAside(t *testing.T) {
	bad := acct("bad", 0, 0)
	bad.Limits[1].Bad = true
	ok := acct("ok", 50, 60)
	d := chosen(t, []Candidate{bad, ok}, Ledger{}, Intent{})
	if d.Chosen != "ok" {
		t.Fatalf("chose %q; a row nothing bounds must not read as room", d.Chosen)
	}
	if c, _ := d.Find("bad"); !c.NearLimit || c.Limits[1].Basis != BasisBad {
		t.Errorf("bad row: near %v basis %s", c.NearLimit, c.Limits[1].Basis)
	}
}

func TestNeverObservedAndNoLimitRowsAreTried(t *testing.T) {
	never := Candidate{Name: "never", Key: "n"}
	empty := Candidate{Name: "empty", Key: "e", ObservedAt: at(-time.Minute)}
	busy := acct("busy", 30, 10)
	d := chosen(t, []Candidate{busy, never, empty}, Ledger{}, Intent{})
	if d.Chosen != "never" {
		t.Fatalf("chose %q, want the untried account first in order", d.Chosen)
	}
	for _, name := range []string{"never", "empty"} {
		if c, _ := d.Find(name); c.Load != 0 || c.NearLimit {
			t.Errorf("%s: load %d near %v", name, c.Load, c.NearLimit)
		}
	}
}

// The draft rule took room as 100 minus the highest row, and a weekly figure
// then hid every ten points a launch added to the session window: all four of
// these went to the first account.
func TestAWeeklyFigureNeverHidesALaunch(t *testing.T) {
	cands := []Candidate{acct("a", 0, 40), acct("b", 0, 50), acct("c", 0, 60), acct("d", 0, 70)}
	var ledger Ledger
	var got []string
	for i := range cands {
		d := chosen(t, cands, ledger, Intent{})
		got = append(got, d.Chosen)
		ledger.Record("uuid:"+d.Chosen, d.Chosen, 100+i, now)
	}
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("four launches went to %v, want four accounts", got)
	}
}

func TestBusySessionsAndUsageAreOneScale(t *testing.T) {
	loaded := acct("loaded", 9, 0)
	for pid := 1; pid <= 5; pid++ {
		loaded.Busy = append(loaded.Busy, Proc{PID: pid, StartedMS: now.UnixMilli() - 3_600_000})
	}
	quiet := acct("quiet", 11, 0)
	d := chosen(t, []Candidate{loaded, quiet}, Ledger{}, Intent{})
	if d.Chosen != "quiet" {
		t.Fatalf("chose %q: 9%% with five busy sessions must lose to 11%% with none", d.Chosen)
	}
	if c, _ := d.Find("loaded"); c.Load != 5 {
		t.Errorf("load = %d, want 5", c.Load)
	}
}

func TestAOnePointDifferenceDoesNotOutrankWeeklyRoom(t *testing.T) {
	d := chosen(t, []Candidate{acct("tight", 0, 42), acct("roomy", 1, 3)}, Ledger{}, Intent{})
	if d.Chosen != "roomy" {
		t.Fatalf("chose %q: both are load 0, so weekly room decides", d.Chosen)
	}
}

func TestTiesGoToWeeklyThenLeastRecentlyPlacedThenOrder(t *testing.T) {
	cands := []Candidate{acct("a", 0, 10), acct("b", 0, 10), acct("c", 0, 10)}
	old := now.Add(-time.Hour).UnixMilli()
	ledger := Ledger{Last: map[string]Last{
		"uuid:a": {Name: "a", AtMS: old + 2},
		"uuid:b": {Name: "b", AtMS: old + 1},
		"uuid:c": {Name: "c", AtMS: old + 1},
	}}
	if d := chosen(t, cands, ledger, Intent{}); d.Chosen != "b" || d.RunnerUp != "c" {
		t.Fatalf("chose %q then %q, want b then c", d.Chosen, d.RunnerUp)
	}
	if d := chosen(t, cands, Ledger{}, Intent{}); d.Chosen != "a" {
		t.Fatalf("with nothing placed, order decides: chose %q", d.Chosen)
	}
}

func TestAPendingLaunchCountsForItsSpanAndOnce(t *testing.T) {
	a, b := acct("a", 0, 0), acct("b", 0, 5)
	recent := now.Add(-5 * time.Minute)
	ledger := Ledger{}
	ledger.Record("uuid:a", "a", 77, recent)

	// The process has exited and the figures were refreshed since: it still
	// counts until its span has passed.
	if d := chosen(t, []Candidate{a, b}, ledger, Intent{}); d.Chosen != "b" {
		t.Fatalf("chose %q with a launch pending on a", d.Chosen)
	}
	// The same launch, now a busy session of that account: one step, not two.
	a.Busy = []Proc{{PID: 77, StartedMS: recent.UnixMilli() + 900}}
	if c, _ := chosen(t, []Candidate{a, b}, ledger, Intent{}).Find("a"); c.Load != 1 || c.Busy != 1 || c.Pending != 0 {
		t.Fatalf("load %d busy %d pending %d, want one step", c.Load, c.Busy, c.Pending)
	}
	// A recycled pid — a session that started long before the launch — is not it.
	a.Busy = []Proc{{PID: 77, StartedMS: recent.UnixMilli() - 3_600_000}}
	if c, _ := chosen(t, []Candidate{a, b}, ledger, Intent{}).Find("a"); c.Load != 2 {
		t.Fatalf("load %d, want 2: another session's pid", c.Load)
	}
	// Past its span it is gone.
	later := Choose([]Candidate{acct("a", 0, 0), b}, ledger, Intent{}, recent.Add(PendingFor+time.Second))
	if c, _ := later.Find("a"); c.Pending != 0 {
		t.Fatalf("pending %d after its span", c.Pending)
	}
}

func TestEveryCandidateNearALimit(t *testing.T) {
	a := acct("a", 10, 92)
	b := acct("b", 85, 20)
	b.Limits[0].ResetAt = at(30 * time.Minute)
	c := acct("c", 10, 85)
	c.Limits[1].ResetAt = at(2 * time.Hour)
	d := chosen(t, []Candidate{a, b, c}, Ledger{}, Intent{})
	if d.Chosen != "b" || d.Reason != ReasonNearLimit {
		t.Fatalf("chose %q (%s): at equal highest row the sooner reset wins", d.Chosen, d.Reason)
	}
	// One account with room takes the launch, whatever its load.
	open := acct("open", 70, 70)
	if d := chosen(t, []Candidate{a, b, open}, Ledger{}, Intent{}); d.Chosen != "open" || d.Reason != ReasonLeastLoad {
		t.Fatalf("chose %q (%s)", d.Chosen, d.Reason)
	}
}

func TestExcludedAccountsAndRefusal(t *testing.T) {
	out := acct("out", 0, 0)
	out.Excluded = "not logged in"
	blocked := acct("blocked", 0, 0)
	blocked.Excluded = "blocked by the vendor"
	if d := chosen(t, []Candidate{out, acct("in", 60, 60), blocked}, Ledger{}, Intent{}); d.Chosen != "in" {
		t.Fatalf("chose %q", d.Chosen)
	}
	d := chosen(t, []Candidate{out, blocked}, Ledger{}, Intent{})
	if d.Chosen != "" || !strings.Contains(d.Refusal, "out (not logged in)") || !strings.Contains(d.Refusal, "blocked (blocked by the vendor)") {
		t.Fatalf("refusal = %q", d.Refusal)
	}
	if d := chosen(t, nil, Ledger{}, Intent{}); d.Chosen != "" || d.Refusal == "" {
		t.Fatalf("no candidates must refuse: %+v", d)
	}
}

func TestANamedSessionFollowsItsOwnerWhileItHasRoom(t *testing.T) {
	owner, other := acct("owner", 60, 60), acct("other", 0, 0)
	if d := chosen(t, []Candidate{owner, other}, Ledger{}, Intent{Owner: "owner"}); d.Chosen != "owner" || d.Reason != ReasonOwner || d.RunnerUp != "other" {
		t.Fatalf("chose %q (%s), next %q", d.Chosen, d.Reason, d.RunnerUp)
	}
	owner.Limits[1].Percent = 90
	if d := chosen(t, []Candidate{owner, other}, Ledger{}, Intent{Owner: "owner"}); d.Chosen != "other" || d.Reason != ReasonMoved {
		t.Fatalf("an owner near a limit is left: chose %q (%s)", d.Chosen, d.Reason)
	}
	if d := chosen(t, []Candidate{other}, Ledger{}, Intent{Owner: "gone"}); d.Chosen != "other" || d.Reason != ReasonMoved {
		t.Fatalf("a vanished owner: chose %q (%s)", d.Chosen, d.Reason)
	}
	// Moving a session means somewhere other than where it is.
	d := chosen(t, []Candidate{acct("here", 0, 0), acct("there", 50, 50)}, Ledger{}, Intent{Exclude: "here"})
	if d.Chosen != "there" {
		t.Fatalf("chose %q", d.Chosen)
	}
	d = chosen(t, []Candidate{acct("here", 0, 0)}, Ledger{}, Intent{Exclude: "here"})
	if d.Chosen != "" || !strings.Contains(d.Refusal, "already there") {
		t.Fatalf("refusal = %q", d.Refusal)
	}
}

func TestRotationWhenNothingWasEverObserved(t *testing.T) {
	cands := []Candidate{{Name: "a", Key: "a"}, {Name: "b", Key: "b"}, {Name: "c", Key: "c"}}
	var ledger Ledger
	var got []string
	clock := now
	for i := 0; i < 6; i++ {
		d := Choose(cands, ledger, Intent{}, clock)
		if d.Reason != ReasonRotation {
			t.Fatalf("reason = %q", d.Reason)
		}
		got = append(got, d.Chosen)
		ledger.Record(d.Chosen, d.Chosen, i, clock)
		clock = clock.Add(20 * time.Minute)
	}
	if strings.Join(got, "") != "abcabc" {
		t.Fatalf("rotation = %v", got)
	}
}

func TestForcedAndLast(t *testing.T) {
	hard := acct("hard", 0, 0)
	hard.Excluded, hard.Unlaunchable = "sessions link is broken", true
	soft := acct("soft", 95, 95)
	soft.Excluded = "not logged in"
	cands := []Candidate{hard, soft, acct("ok", 0, 0)}

	// A forced choice is the caller's: an account an automatic launch avoids is
	// still launched — that is how one logs in.
	if d := chosen(t, cands, Ledger{}, Intent{Kind: Forced, Account: "soft", Reason: "named"}); d.Chosen != "soft" || d.Reason != "named" {
		t.Fatalf("forced: %+v", d)
	}
	if d := chosen(t, cands, Ledger{}, Intent{Kind: Forced, Account: "hard"}); d.Chosen != "" || !strings.Contains(d.Refusal, "sessions link") {
		t.Fatalf("forced unlaunchable: %+v", d)
	}
	if d := chosen(t, cands, Ledger{}, Intent{Kind: Forced, Account: "nobody"}); d.Chosen != "" {
		t.Fatalf("forced unknown: %+v", d)
	}

	if d := chosen(t, cands, Ledger{}, Intent{Kind: LastUsed}); d.Chosen != "" || !strings.Contains(d.Refusal, "no launch is recorded") {
		t.Fatalf("last with nothing recorded: %+v", d)
	}
	var ledger Ledger
	ledger.Record("uuid:ok", "ok", 1, now.Add(-3*time.Hour))
	ledger.Record("uuid:soft", "soft", 2, now.Add(-2*time.Hour))
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed}); d.Chosen != "soft" || d.Reason != ReasonLast {
		t.Fatalf("last: %+v", d)
	}
	ledger.Record("uuid:gone", "gone", 3, now.Add(-time.Hour))
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed}); d.Chosen != "" || !strings.Contains(d.Refusal, "gone") {
		t.Fatalf("last names a removed account: %+v", d)
	}
}

func TestPruneKeepsWhatStillCounts(t *testing.T) {
	var l Ledger
	l.Record("a", "a", 1, now.Add(-20*time.Minute))
	l.Record("b", "b", 2, now.Add(-5*time.Minute))
	l.Record("c", "c", 3, now.Add(-40*24*time.Hour))
	if !l.Prune(now, 30*24*time.Hour) {
		t.Fatal("nothing pruned")
	}
	if len(l.Recent) != 1 || l.Recent[0].Name != "b" {
		t.Fatalf("recent = %+v", l.Recent)
	}
	if _, ok := l.Last["c"]; ok || len(l.Last) != 2 {
		t.Fatalf("last = %+v", l.Last)
	}
	if last, ok := l.Newest(); !ok || last.Name != "b" {
		t.Fatalf("newest = %+v", last)
	}
	if l.Prune(now, 30*24*time.Hour) {
		t.Fatal("a second prune changed something")
	}
}
