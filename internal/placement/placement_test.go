package placement

import (
	"math"
	"slices"
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
	if got.Load != 0 || got.NearLimit {
		t.Errorf("load %d near %v", got.Load, got.NearLimit)
	}
	// 62 points of room ending tomorrow is a looser bound than 76 points over
	// a whole week nobody gave a reset for: the scoped row is the week.
	if w := got.Week; w.Kind != "weekly_scoped" || w.Counted != 4 || w.Basis != BasisStale || w.LeftBasis != TimeAssumed {
		t.Errorf("week = %+v", w)
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
		ledger.Record("uuid:"+d.Chosen, d.Chosen, 100+i, here, now)
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

// weekly builds a candidate observed just now, its session window empty and
// its one weekly row ending after left.
func weekly(name string, percent int, left time.Duration) Candidate {
	return Candidate{
		Name: name, Key: "uuid:" + name, ObservedAt: at(-time.Minute),
		Limits: []Limit{
			{Kind: "session", Label: "5h session", Percent: 0, Session: true},
			{Kind: "weekly_all", Label: "All models (7d)", Percent: percent, ResetAt: at(left)},
		},
	}
}

// The owner's accounts on 2026-10-05: weekly room that ends sooner is spent
// first, and a burst still spreads, because each placement is load.
func TestWeeklyRoomThatEndsSoonerGoesFirst(t *testing.T) {
	cands := []Candidate{
		weekly("qiushiyan", 10, 92*time.Hour),
		weekly("yqs", 0, 151*time.Hour),
		weekly("yan", 0, 94*time.Hour),
		weekly("qiushi", 32, 80*time.Minute),
		weekly("cliushi", 7, 45*time.Hour),
		weekly("qiushi@", 0, 74*time.Hour),
	}
	var ledger Ledger
	var got []string
	for i := range cands {
		d := chosen(t, cands, ledger, Intent{})
		got = append(got, d.Chosen)
		ledger.Record("uuid:"+d.Chosen, d.Chosen, 100+i, here, now)
	}
	if want := "qiushi,cliushi,qiushi@,yan,qiushiyan,yqs"; strings.Join(got, ",") != want {
		t.Fatalf("six launches went to %v, want %s", got, want)
	}
}

// A weekly row is judged against its own window. Judged by its highest
// percent first, A would lose to B, then spend one more point on the row that
// resets tomorrow and win — more spending making an account more attractive.
func TestSpendingMoreNeverMakesAnAccountMoreAttractive(t *testing.T) {
	a := func(tomorrow int) Candidate {
		return Candidate{
			Name: "a", Key: "uuid:a", ObservedAt: at(-time.Minute),
			Limits: []Limit{
				{Kind: "session", Percent: 0, Session: true},
				{Kind: "weekly_all", Percent: 60, ResetAt: at(6 * 24 * time.Hour)},
				{Kind: "weekly_scoped", Percent: tomorrow, ResetAt: at(24 * time.Hour)},
			},
		}
	}
	b := weekly("b", 40, 4*24*time.Hour)
	for _, tomorrow := range []int{59, 61, 75} {
		d := chosen(t, []Candidate{a(tomorrow), b}, Ledger{}, Intent{})
		if d.Chosen != "b" {
			t.Errorf("tomorrow's row at %d%%: chose %q, want b", tomorrow, d.Chosen)
		}
		if c, _ := d.Find("a"); c.Week.Kind != "weekly_all" || c.Week.Room != 20 {
			t.Errorf("tomorrow's row at %d%%: a's week = %+v, want the six-day row", tomorrow, c.Week)
		}
	}
}

func TestLoadStillRanksBeforeTheWeek(t *testing.T) {
	ending := weekly("ending", 0, time.Hour)
	ending.Limits[0].Percent = 10
	d := chosen(t, []Candidate{ending, weekly("fresh", 70, 6*24*time.Hour)}, Ledger{}, Intent{})
	if d.Chosen != "fresh" {
		t.Fatalf("chose %q: a step of five-hour load outranks any weekly figure", d.Chosen)
	}
}

// One session at a time returns to the room that ends sooner once the last
// launch has stopped counting: that is the preference, not a pile-up. Inside
// the span the launch is load, and the next goes elsewhere.
func TestSerialUseReturnsToTheSoonerReset(t *testing.T) {
	cands := []Candidate{weekly("later", 0, 6*24*time.Hour), weekly("sooner", 0, 24*time.Hour)}
	var ledger Ledger
	ledger.Record("uuid:sooner", "sooner", 1, here, now)
	if d := Choose(cands, ledger, Intent{}, now.Add(5*time.Minute)); d.Chosen != "later" {
		t.Errorf("five minutes on: chose %q, want later", d.Chosen)
	}
	if d := Choose(cands, ledger, Intent{}, now.Add(20*time.Minute)); d.Chosen != "sooner" {
		t.Errorf("twenty minutes on: chose %q, want sooner", d.Chosen)
	}
}

// Time left is the vendor's reset while it is ahead; past it, or without one,
// a whole window — the stated one, else AssumedWindow — and it says which.
func TestTheTimeAWeekHasLeft(t *testing.T) {
	assumed := int64(AssumedWindow / time.Second)
	row := func(reset int64, window int64) Candidate {
		return Candidate{Name: "a", Key: "a", ObservedAt: at(-time.Minute), Limits: []Limit{
			{Kind: "weekly", Percent: 20, ResetAt: reset, Window: window},
		}}
	}
	// A weekly window on Claude Code's fixed seven-day schedule, 20% spent
	// while its figures were taken.
	sched := func(reset int64) Candidate {
		c := row(reset, 0)
		c.Limits[0].Period = 604_800
		return c
	}
	cases := []struct {
		name  string
		c     Candidate
		left  int64
		basis TimeBasis
		room  int
	}{
		{"reset ahead", row(at(30*time.Hour), 604_800), 30 * 3600, TimeReset, 60},
		{"reset one second ahead", row(at(time.Second), 0), 1, TimeReset, 60},
		{"reset at this instant has passed", row(at(0), 0), assumed, TimeAssumed, 80},
		{"passed on a schedule, this instant", sched(at(0)), 604_800, TimeProjected, 80},
		{"passed on a schedule, 13 h ago", sched(at(-13 * time.Hour)), 155 * 3600, TimeProjected, 80},
		{"passed on a schedule, 8 days ago", sched(at(-8 * 24 * time.Hour)), 6 * 86_400, TimeProjected, 80},
		{"ahead on a schedule", sched(at(30 * time.Hour)), 30 * 3600, TimeReset, 60},
		{"no reset on a schedule", sched(0), assumed, TimeAssumed, 60},
		{"passed, window stated", row(at(-time.Hour), 2*86_400), 2 * 86_400, TimeWindow, 80},
		{"unstarted, window stated", row(0, 604_800), 604_800, TimeWindow, 60},
		{"no reset, no window", row(0, 0), assumed, TimeAssumed, 60},
		{"no weekly row", Candidate{Name: "a", Key: "a", ObservedAt: at(-time.Minute)}, assumed, TimeAssumed, 80},
		{"never observed", Candidate{Name: "a", Key: "a"}, assumed, TimeAssumed, 80},
	}
	for _, c := range cases {
		w := chosen(t, []Candidate{c.c}, Ledger{}, Intent{}).Candidates[0].Week
		if w.Left != c.left || w.LeftBasis != c.basis || w.Room != c.room {
			t.Errorf("%s: left %d/%s room %d, want %d/%s room %d", c.name, w.Left, w.LeftBasis, w.Room, c.left, c.basis, c.room)
		}
	}
	// Knowing nothing is never more pressing than room the vendor says ends.
	d := chosen(t, []Candidate{{Name: "unknown", Key: "u"}, weekly("known", 0, 6*24*time.Hour+23*time.Hour)}, Ledger{}, Intent{})
	if d.Chosen != "known" {
		t.Errorf("chose %q, want the account whose room ends", d.Chosen)
	}
}

// An idle account's figures can be days old. Its weekly window ended 13 h ago
// and, on its seven-day schedule, renews again in 155 h: sooner than b's 166 h,
// so it takes the tie — a whole week from now would have put it behind b.
func TestAPassedResetNamesTheNextOnItsSchedule(t *testing.T) {
	a := Candidate{Name: "a", Key: "a", ObservedAt: at(-14 * time.Hour), Limits: []Limit{
		{Kind: "session", Percent: 0, Session: true},
		{Kind: "weekly_all", Percent: 60, ResetAt: at(-13 * time.Hour), Period: 604_800},
	}}
	d := chosen(t, []Candidate{a, weekly("b", 0, 166*time.Hour)}, Ledger{}, Intent{})
	if d.Chosen != "a" {
		t.Fatalf("chose %q; a renews in 155 h, b in 166 h", d.Chosen)
	}
	if c, _ := d.Find("a"); c.Week.Counted != 0 || c.Week.Basis != BasisEnded || c.Week.LeftBasis != TimeProjected {
		t.Errorf("a's week = %+v", c.Week)
	}
}

func TestFiguresAtTheirLimitsCompareExactly(t *testing.T) {
	huge := Candidate{Name: "huge", Key: "h", ObservedAt: at(-time.Minute), Limits: []Limit{
		{Kind: "weekly", Percent: -50, ResetAt: math.MaxInt64},
		{Kind: "weekly", Percent: 0, Window: math.MaxInt64},
	}}
	tight := weekly("tight", 79, 24*time.Hour)
	d := chosen(t, []Candidate{huge, tight}, Ledger{}, Intent{})
	if d.Chosen != "tight" {
		t.Fatalf("chose %q: one point over a day is more pressing than any room over forever", d.Chosen)
	}
	if c, _ := d.Find("huge"); c.Week.Room != NearLimitPercent {
		t.Errorf("room = %d, want it held to %d", c.Week.Room, NearLimitPercent)
	}
	x := Week{Room: 80, Left: math.MaxInt64}
	y := Week{Room: 79, Left: math.MaxInt64 - 1}
	if x.Pressing(y) != -y.Pressing(x) || x.Pressing(x) != 0 {
		t.Error("Pressing is not antisymmetric at the extremes")
	}
}

func TestAPendingLaunchCountsForItsSpanAndOnce(t *testing.T) {
	a, b := acct("a", 0, 0), acct("b", 0, 5)
	recent := now.Add(-5 * time.Minute)
	ledger := Ledger{}
	ledger.Record("uuid:a", "a", 77, here, recent)

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
		ledger.Record(d.Chosen, d.Chosen, i, here, clock)
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

	if d := chosen(t, cands, Ledger{}, Intent{Kind: LastUsed, Home: here}); d.Chosen != "" || !strings.Contains(d.Refusal, "no launch is recorded") {
		t.Fatalf("last with nothing recorded: %+v", d)
	}
	var ledger Ledger
	ledger.Record("uuid:ok", "ok", 1, here, now.Add(-3*time.Hour))
	ledger.Record("uuid:soft", "soft", 2, here, now.Add(-2*time.Hour))
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed, Home: here}); d.Chosen != "soft" || d.Reason != ReasonLast {
		t.Fatalf("last: %+v", d)
	}
	ledger.Record("uuid:gone", "gone", 3, here, now.Add(-time.Hour))
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed, Home: here}); d.Chosen != "" || !strings.Contains(d.Refusal, "gone") {
		t.Fatalf("last names a removed account: %+v", d)
	}
}

func TestPruneKeepsWhatStillCounts(t *testing.T) {
	var l Ledger
	l.Record("a", "a", 1, here, now.Add(-20*time.Minute))
	l.Record("b", "b", 2, here, now.Add(-5*time.Minute))
	l.Record("c", "c", 3, here, now.Add(-40*24*time.Hour))
	if !l.Prune(now, 30*24*time.Hour) {
		t.Fatal("nothing pruned")
	}
	if len(l.Recent) != 1 || l.Recent[0].Name != "b" {
		t.Fatalf("recent = %+v", l.Recent)
	}
	if _, ok := l.Last["c"]; ok || len(l.Last) != 2 {
		t.Fatalf("last = %+v", l.Last)
	}
	if last, ok := l.Newest(here); !ok || last.Name != "b" {
		t.Fatalf("newest = %+v", last)
	}
	if l.Prune(now, 30*24*time.Hour) {
		t.Fatal("a second prune changed something")
	}
}

// here is the home a test's launches are made from.
const here = "/home/owner/.claude-accounts"

// Two homes holding logins of one subscription spend one quota: a session busy
// in the other home and a launch the other home made both count on the
// subscription, once each, and the count says whose they are.
func TestAnotherHomesSessionsAndLaunchesCountOnTheSubscription(t *testing.T) {
	const there = "/home/steward/.claude-accounts"
	shared, spare := acct("shared", 0, 0), acct("spare", 0, 5)
	started := now.Add(-time.Hour).UnixMilli()
	shared.Busy = []Proc{{PID: 10, StartedMS: started, Home: there}, {PID: 11, StartedMS: started, Home: here}}
	var ledger Ledger
	ledger.Record("uuid:shared", "shared", 20, there, now.Add(-time.Minute))
	// A launch the other home made two minutes ago that has since become one
	// of its busy sessions is counted once, as the session: one pid space.
	ledger.Record("uuid:shared", "shared", 10, there, now.Add(-2*time.Minute))
	shared.Busy[0].StartedMS = now.Add(-2 * time.Minute).UnixMilli()

	d := chosen(t, []Candidate{shared, spare}, ledger, Intent{Home: here})
	if d.Chosen != "spare" {
		t.Fatalf("chose %q: the other home's load is on that subscription", d.Chosen)
	}
	c, _ := d.Find("shared")
	if c.Busy != 2 || c.Pending != 1 || c.Load != 3 {
		t.Errorf("busy %d pending %d load %d, want 2, 1, 3", c.Busy, c.Pending, c.Load)
	}
	want := []Share{{Home: here, Busy: 1}, {Home: there, Busy: 1, Pending: 1}}
	if !slices.Equal(c.Shares, want) {
		t.Errorf("shares = %+v, want %+v", c.Shares, want)
	}
	if got := c.Elsewhere(here); !slices.Equal(got, want[1:]) {
		t.Errorf("elsewhere = %+v", got)
	}
}

// An account name is one home's: the last account another home used is never
// this home's last, even when both homes name a dir alike.
func TestTheLastAccountUsedIsPerHome(t *testing.T) {
	const there = "/home/steward/.claude-accounts"
	cands := []Candidate{acct("a", 0, 0), acct("b", 0, 0)}
	var ledger Ledger
	ledger.Record("uuid:a", "a", 1, here, now.Add(-time.Hour))
	ledger.Record("uuid:b", "b", 2, there, now.Add(-time.Minute))
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed, Home: here}); d.Chosen != "a" {
		t.Fatalf("this home's last = %q, want a", d.Chosen)
	}
	if d := chosen(t, cands, ledger, Intent{Kind: LastUsed, Home: "/elsewhere"}); d.Chosen != "" {
		t.Fatalf("a home that never launched has a last account: %+v", d)
	}
	// The tie-break is the subscription's, whichever home placed it.
	if ledger.Last["uuid:b"].Name != "b" {
		t.Errorf("last per subscription = %+v", ledger.Last)
	}
}
