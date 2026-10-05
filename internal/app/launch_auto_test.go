package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// autoFixture is a home with a primary and extras, each logged in, each with a
// valid shared-sessions topology, and the launch path's edges replaced: no
// Keychain, no process table, no detached child.
type autoFixture struct {
	t         *testing.T
	cfg       config.Scope
	st        *state.Store
	refreshes *int
	live      map[int]int64 // pid → kernel start, for the registry probe
}

const (
	sessA = "11111111-1111-4111-8111-111111111111"
	sessB = "22222222-2222-4222-8222-222222222222"
)

func newAutoFixture(t *testing.T, extras ...string) autoFixture {
	t.Helper()
	home, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := claudeScope(home, "qiushi")
	if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"qiushi@x.com","accountUuid":"u-qiushi"}}`)
	for _, name := range extras {
		dir := filepath.Join(cfg.AccountsRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, name, name))
		if err := os.Symlink(cfg.StoreDir(), filepath.Join(dir, "projects")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	f := autoFixture{t: t, cfg: cfg, st: state.Open(cfg), refreshes: new(int), live: map[int]int64{}}
	prevCreds, prevProbe, prevRefresh := placementCreds, placementProbe, startRefresh
	far := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	placementCreds = func(accounts.Account) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, far, far)
	}
	placementProbe = func(pid int) (int64, error) {
		if start, ok := f.live[pid]; ok {
			return start, nil
		}
		return 0, os.ErrNotExist
	}
	startRefresh = func(config.Scope) error { *f.refreshes++; return nil }
	t.Cleanup(func() { placementCreds, placementProbe, startRefresh = prevCreds, prevProbe, prevRefresh })
	return f
}

func (f autoFixture) key(name string) state.Key {
	for _, a := range accounts.Discover(f.cfg).Accounts {
		if a.Name == name {
			return state.Key{UUID: a.AccountID, Name: a.Name}
		}
	}
	f.t.Fatalf("no account %q", name)
	return state.Key{}
}

// observe stores a usage response for an account, through the store's own
// claim and completion, as taken `age` ago.
func (f autoFixture) observe(name string, session, weekly int, age time.Duration) {
	f.t.Helper()
	f.observeWeek(name, session, weekly, 72*time.Hour, age)
}

// observeWeek is observe with the weekly window ending `left` from now.
func (f autoFixture) observeWeek(name string, session, weekly int, left, age time.Duration) {
	f.t.Helper()
	at := time.Now().Add(-age)
	reset := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	body := fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":%d,"resets_at":%q},`+
		`{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":%q}]}`, session, reset(2*time.Hour), weekly, reset(left))
	k := f.key(name)
	dec, err := f.st.Claim([]state.Key{k}, at)
	if err != nil || !dec[0].Permit {
		f.t.Fatalf("claim %s: %+v %v", name, dec, err)
	}
	if _, err := f.st.Complete(k, dec[0].Generation, state.OutcomeStored, []byte(body), at); err != nil {
		f.t.Fatal(err)
	}
}

func (f autoFixture) setCurrent(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.cfg.CurrentFile(), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// launched is where one launch went: the account the child environment selects.
type launched struct {
	code   int
	called bool
	args   []string
	env    []string
	stderr string
}

func (l launched) account(cfg config.Scope) string {
	for _, kv := range l.env {
		if dir, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return filepath.Base(dir)
		}
	}
	return "qiushi" // the primary is selected by the variable's absence
}

func (f autoFixture) launch(args ...string) launched {
	f.t.Helper()
	var out launched
	prev := execVendor
	execVendor = func(path, binary string, vendorArgs, environ []string) error {
		out.called, out.args, out.env = true, vendorArgs, environ
		return nil
	}
	defer func() { execVendor = prev }()
	out.stderr = captureStderr(f.t, func() { out.code = runLaunch(f.cfg, args) })
	return out
}

func (f autoFixture) log() []launchlog.Record {
	f.t.Helper()
	recs, skipped, err := launchlog.Read(f.cfg.AccountsRoot, 0)
	if err != nil || skipped != 0 {
		f.t.Fatalf("log: %d skipped, %v", skipped, err)
	}
	return recs
}

func TestAutoLaunchChoosesTheLeastLoadedAccountAndSaysSo(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 34, 40, 3*time.Minute)
	f.observe("a@x.com", 4, 30, 3*time.Minute)
	f.observe("b@x.com", 2, 60, 3*time.Minute)

	got := f.launch("--", "-p", "hello")
	if got.code != 0 || !got.called {
		t.Fatalf("exit %d called %v: %s", got.code, got.called, got.stderr)
	}
	if got.account(f.cfg) != "a@x.com" {
		t.Fatalf("launched on %s: a and b are both load 0, and a has more weekly room\n%s", got.account(f.cfg), got.stderr)
	}
	if !slices.Equal(got.args, []string{"-p", "hello"}) {
		t.Errorf("vendor args = %v", got.args)
	}
	for _, want := range []string{"headroom launch: a@x.com · auto", "5h 4%", "week 30%", "load 0", "(next: b@x.com)"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("launch line lacks %q:\n%s", want, got.stderr)
		}
	}
	ledger := f.st.Load().Placements()
	if len(ledger.Recent) != 1 || ledger.Recent[0].Name != "a@x.com" || ledger.Recent[0].PID != os.Getpid() {
		t.Errorf("placement record = %+v", ledger.Recent)
	}
	recs := f.log()
	if len(recs) != 1 || recs[0].Mode != "auto" || recs[0].Reason != placement.ReasonLeastLoad ||
		recs[0].Chosen != "a@x.com" || !recs[0].Recorded || len(recs[0].Candidates) != 3 {
		t.Errorf("log = %+v", recs)
	}
	if *f.refreshes != 1 {
		t.Errorf("%d refreshes started, want one for the next launch", *f.refreshes)
	}
	if data, _ := os.ReadFile(f.cfg.CurrentFile()); string(data) != "auto\n" {
		t.Errorf(".current = %q", data)
	}
}

// The refresh a launch leaves behind is for the next launch, and only when
// some account may be asked: inside every account's quiet period the claim
// would refuse them all, and the process is not started.
func TestNoRefreshIsStartedInsideTheQuietPeriod(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 0, 0, 10*time.Second)
	f.observe("a@x.com", 0, 0, 10*time.Second)
	if got := f.launch(); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if *f.refreshes != 0 {
		t.Errorf("%d refreshes started with every account inside its quiet period", *f.refreshes)
	}

	// Past the quiet period but with every stored token aged out, nobody can
	// be asked either: an idle machine must not spawn a process per launch to
	// learn that again.
	stale := newAutoFixture(t, "a@x.com")
	stale.setCurrent("auto\n")
	stale.observe("qiushi", 0, 0, time.Hour)
	stale.observe("a@x.com", 0, 0, time.Hour)
	past := time.Now().Add(-time.Hour).UnixMilli()
	far := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	placementCreds = func(accounts.Account) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, past, far)
	}
	if got := stale.launch(); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if *stale.refreshes != 0 {
		t.Errorf("%d refreshes started with no askable account", *stale.refreshes)
	}
}

func TestAutoLaunchesSpreadAcrossAccounts(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com")
	f.setCurrent("auto\n")
	for _, name := range []string{"qiushi", "a@x.com", "b@x.com", "c@x.com"} {
		f.observe(name, 0, 10, time.Minute)
	}
	seen := map[string]int{}
	for range 4 {
		got := f.launch()
		if got.code != 0 {
			t.Fatalf("exit %d: %s", got.code, got.stderr)
		}
		seen[got.account(f.cfg)]++
	}
	if len(seen) != 4 {
		t.Fatalf("four launches against four equal accounts went to %v", seen)
	}
}

// An account that cannot be asked is tried, with figures that say they are
// old, and its own placement keeps the next launch from following it there.
func TestAStaleAccountIsTriedOnceAndSaysItsFiguresAreOld(t *testing.T) {
	f := newAutoFixture(t, "idle@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 3, 20, time.Minute)
	f.observe("idle@x.com", 0, 5, 40*time.Hour)

	first := f.launch()
	if first.account(f.cfg) != "idle@x.com" || !strings.Contains(first.stderr, "figures 1d old") {
		t.Fatalf("first launch went to %s:\n%s", first.account(f.cfg), first.stderr)
	}
	if second := f.launch(); second.account(f.cfg) != "qiushi" {
		t.Fatalf("second launch followed the first onto unverified figures:\n%s", second.stderr)
	}
	recs := f.log()
	for _, c := range recs[0].Candidates {
		if c.Name == "idle@x.com" && c.Limits[1].Basis != "stale" {
			t.Errorf("an old figure was logged as %q", c.Limits[1].Basis)
		}
	}
}

// Pinned mode routes as it did: same account, same environment, same argv. It
// is recorded — a pinned launch is load an automatic one must see — and it
// asks nothing of the network.
func TestPinnedLaunchRoutesAsBeforeAndIsRecorded(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("a@x.com\n")
	t.Setenv("CLAUDE_CONFIG_DIR", "/leaked/other-account")

	got := f.launch("--", "--model", "x")
	if got.code != 0 || got.account(f.cfg) != "a@x.com" || !slices.Equal(got.args, []string{"--model", "x"}) {
		t.Fatalf("pinned launch: %+v", got)
	}
	n := 0
	for _, kv := range got.env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d CLAUDE_CONFIG_DIR entries, want exactly 1", n)
	}
	if !strings.Contains(got.stderr, "a@x.com · pinned") {
		t.Errorf("a forgotten pin must be visible at every launch:\n%s", got.stderr)
	}
	if recs := f.log(); len(recs) != 1 || recs[0].Mode != "pinned" || recs[0].Reason != "pinned" {
		t.Errorf("log = %+v", recs)
	}
	if len(f.st.Load().Placements().Recent) != 1 {
		t.Error("the pinned launch was not recorded as load")
	}
	if *f.refreshes != 0 {
		t.Error("a pinned launch started a refresh")
	}
}

// A named launch says where it went, like every other launch, and is load.
func TestNamedLaunchSaysWhereItWentAndCounts(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 0, 50, time.Minute)
	f.observe("a@x.com", 0, 10, time.Minute)

	named := f.launch("--account", "a@x.com")
	if named.code != 0 || named.account(f.cfg) != "a@x.com" || named.stderr != "headroom launch: a@x.com · named\n" {
		t.Fatalf("named launch: %+v", named)
	}
	if data, _ := os.ReadFile(f.cfg.CurrentFile()); string(data) != "auto\n" {
		t.Errorf("a named launch changed the mode: %q", data)
	}
	// a would have won on weekly room; the named launch on it is one step.
	if next := f.launch(); next.account(f.cfg) != "qiushi" {
		t.Fatalf("automatic launch ignored a named launch's load:\n%s", next.stderr)
	}
	if recs := f.log(); recs[0].Mode != "named" || recs[1].Mode != "auto" {
		t.Errorf("log modes = %s, %s", recs[0].Mode, recs[1].Mode)
	}
}

func TestModeIsReadStrictly(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")

	// Absent still means the primary, pinned.
	if got := f.launch(); got.account(f.cfg) != "qiushi" || !strings.Contains(got.stderr, "pinned") {
		t.Fatalf("absent .current: %+v", got)
	}
	for name, content := range map[string]string{"empty": "", "unknown": "gone@x.com\n", "near miss": "Auto\n"} {
		f.setCurrent(content)
		if got := f.launch(); got.code != 1 || got.called {
			t.Errorf("%s .current: exit %d called %v", name, got.code, got.called)
		}
	}

	// The word names the mode and nothing else: an account bearing it makes the
	// file ambiguous, and ambiguity refuses.
	clash := filepath.Join(f.cfg.AccountsRoot, "auto")
	if err := os.MkdirAll(clash, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setCurrent("auto\n")
	got := f.launch()
	if got.code != 1 || got.called || !strings.Contains(got.stderr, clash) {
		t.Fatalf("auto beside an account named auto: %+v", got)
	}
	set := accounts.Discover(f.cfg)
	if err := set.SetAuto(); err == nil {
		t.Error("SetAuto succeeded beside an account named auto")
	}
	if set.Routing().Mode != "" {
		t.Errorf("mode = %q, want unresolvable", set.Routing().Mode)
	}
}

func TestExcludedAccountsAreNeverChosen(t *testing.T) {
	f := newAutoFixture(t, "out@x.com", "expired@x.com", "ok@x.com")
	f.setCurrent("auto\n")
	// Parsed, and names nobody: never logged in.
	writeJSON(t, filepath.Join(f.cfg.AccountsRoot, "out@x.com", ".claude.json"), `{}`)
	far := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	placementCreds = func(a accounts.Account) string {
		refresh := far
		if a.Name == "expired@x.com" {
			refresh = time.Now().Add(-time.Hour).UnixMilli()
		}
		if a.Name == "ok@x.com" {
			return "" // unreadable is not evidence: a locked Keychain reads this way for every account
		}
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, far, refresh)
	}
	f.observe("qiushi", 50, 50, time.Minute)

	got := f.launch()
	if got.account(f.cfg) != "ok@x.com" {
		t.Fatalf("launched on %s:\n%s", got.account(f.cfg), got.stderr)
	}
	recs := f.log()
	want := map[string]string{"out@x.com": "not logged in", "expired@x.com": "login expired", "ok@x.com": "", "qiushi": ""}
	for _, c := range recs[0].Candidates {
		if c.Excluded != want[c.Name] {
			t.Errorf("%s excluded %q, want %q", c.Name, c.Excluded, want[c.Name])
		}
	}

	// Naming an account the rule avoids still launches it: that is how one logs in.
	if named := f.launch("--account", "out@x.com"); named.code != 0 || named.account(f.cfg) != "out@x.com" {
		t.Fatalf("named launch on a logged-out account: %+v", named)
	}

	// Nothing left to choose: refuse, and say why for each.
	for _, name := range []string{"qiushi", "ok@x.com"} {
		path := filepath.Join(f.cfg.AccountsRoot, name, ".claude.json")
		if name == "qiushi" {
			path = f.cfg.PrimaryMeta()
		}
		writeJSON(t, path, `{}`)
	}
	refused := f.launch()
	if refused.code != 1 || refused.called || !strings.Contains(refused.stderr, "expired@x.com (login expired)") ||
		!strings.Contains(refused.stderr, "out@x.com (not logged in)") {
		t.Fatalf("with no usable account: %+v", refused)
	}
}

func TestANamedSessionFollowsItsOwner(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 0, 0, time.Minute)
	f.observe("a@x.com", 30, 30, time.Minute)
	f.observe("b@x.com", 60, 60, time.Minute)

	// A first turn names an id nobody owns: it is placed, and the placement is
	// recorded so its later turns can follow.
	first := f.launch("--", "-p", "--session-id", sessA, "hi")
	if first.account(f.cfg) != "qiushi" || !slices.Equal(first.args, []string{"-p", "--session-id", sessA, "hi"}) {
		t.Fatalf("first turn: %+v", first)
	}
	if !strings.Contains(first.stderr, "session had no known account") {
		t.Errorf("a first turn's line does not say the session was new to headroom:\n%s", first.stderr)
	}
	rec, ok := f.st.Load().Owner(sessA)
	if !ok || rec.Account != "qiushi" {
		t.Fatalf("the placed session was not re-homed: %+v", rec)
	}

	// Its second turn follows, though the rule alone would now go elsewhere
	// (qiushi carries the first turn's step), and writes nothing new.
	for _, spelling := range [][]string{{"--resume", sessA}, {"-r", sessA}, {"--resume=" + sessA}} {
		got := f.launch(append([]string{"--"}, spelling...)...)
		if got.account(f.cfg) != "qiushi" || !strings.Contains(got.stderr, "this session's account") {
			t.Fatalf("%v went to %s:\n%s", spelling, got.account(f.cfg), got.stderr)
		}
		if !slices.Equal(got.args, spelling) {
			t.Errorf("vendor args %v, want %v unchanged", got.args, spelling)
		}
	}
	if after, _ := f.st.Load().Owner(sessA); after != rec {
		t.Errorf("following the owner re-stamped its record: %+v → %+v", rec, after)
	}

	// Prompt history is evidence too, as it is in the picker.
	hist := fmt.Sprintf(`{"sessionId":%q,"timestamp":%d}`+"\n", sessB, time.Now().Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(f.cfg.AccountsRoot, "a@x.com", "history.jsonl"), []byte(hist), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.launch("--", "--resume", sessB); got.account(f.cfg) != "a@x.com" {
		t.Fatalf("a session known from prompt history went to %s:\n%s", got.account(f.cfg), got.stderr)
	}
	if _, ok := f.st.Load().Owner(sessB); ok {
		t.Error("following a history owner wrote a re-home")
	}
}

func TestAnOwnerNearALimitIsLeftAndTheMoveIsRecorded(t *testing.T) {
	f := newAutoFixture(t, "full@x.com", "room@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 50, 50, time.Minute)
	f.observe("full@x.com", 10, 91, time.Minute)
	f.observe("room@x.com", 0, 5, time.Minute)
	if err := rehome(f.st, sessA, "full@x.com", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := f.launch("--", "--resume", sessA)
	if got.account(f.cfg) != "room@x.com" || !strings.Contains(got.stderr, "session moved from full@x.com") {
		t.Fatalf("went to %s:\n%s", got.account(f.cfg), got.stderr)
	}
	if rec, _ := f.st.Load().Owner(sessA); rec.Account != "room@x.com" {
		t.Errorf("the move was not recorded: %+v", rec)
	}
	if recs := f.log(); recs[0].Reason != placement.ReasonMoved || recs[0].Session != sessA {
		t.Errorf("log = %+v", recs[0])
	}
}

// A verified live claim outranks every record: that account is driving the
// session at this instant.
func TestALiveClaimOutranksAReHome(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	if err := rehome(f.st, sessA, "a@x.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Hour)
	sdir := filepath.Join(f.cfg.AccountsRoot, "b@x.com", "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(sdir, "4242.json"),
		fmt.Sprintf(`{"pid":4242,"sessionId":%q,"startedAt":%d,"status":"idle"}`, sessA, started.UnixMilli()))
	f.live[4242] = started.Unix()

	if got := f.launch("--", "--resume", sessA); got.account(f.cfg) != "b@x.com" {
		t.Fatalf("went to %s, want the account running it:\n%s", got.account(f.cfg), got.stderr)
	}
}

// Which session `--continue` resumes is the vendor's rule, unverified: nothing
// is inferred, and nothing is recorded about a session.
func TestUnidentifiedSessionsArePlacedAndNothingIsRecorded(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	if err := rehome(f.st, sessA, "a@x.com", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := f.st.Load().Owners()
	for _, args := range [][]string{{"--continue"}, {"-c"}, {"--resume"}, {"--resume", "search words"}, {"--session-id", "not-a-uuid"}} {
		got := f.launch(append([]string{"--"}, args...)...)
		if got.code != 0 || !slices.Equal(got.args, args) {
			t.Fatalf("%v: %+v", args, got)
		}
		if args[0] != "--session-id" && !strings.Contains(got.stderr, "session not identified") {
			t.Errorf("%v: the line does not say the session was not identified:\n%s", args, got.stderr)
		}
	}
	after := f.st.Load().Owners()
	if len(after) != len(before) || after[sessA] != before[sessA] {
		t.Errorf("session records changed: %v → %v", before, after)
	}
	for _, r := range f.log() {
		if r.Session != "" {
			t.Errorf("a guessed session reached the log: %+v", r.Session)
		}
	}
}

func TestSessionIntent(t *testing.T) {
	cases := []struct {
		args []string
		want sessionRef
	}{
		{nil, sessionRef{}},
		{[]string{"-p", "hello"}, sessionRef{}},
		{[]string{"--resume", sessA}, sessionRef{Route: sessA, Record: sessA}},
		{[]string{"-r", sessA, "--model", "x"}, sessionRef{Route: sessA, Record: sessA}},
		{[]string{"--resume=" + sessA}, sessionRef{Route: sessA, Record: sessA}},
		{[]string{"--session-id", sessA}, sessionRef{Route: sessA, Record: sessA}},
		{[]string{"--session-id=" + sessA}, sessionRef{Route: sessA, Record: sessA}},
		// A fork follows the session it forks from and creates another: the
		// original is never recorded as having moved.
		{[]string{"--resume", sessA, "--fork-session"}, sessionRef{Route: sessA}},
		{[]string{"--resume", sessA, "--fork-session", "--session-id", sessB}, sessionRef{Route: sessA}},
		{[]string{"--continue"}, sessionRef{Unidentified: true}},
		{[]string{"-c"}, sessionRef{Unidentified: true}},
		{[]string{"--resume"}, sessionRef{Unidentified: true}},
		{[]string{"--resume", "a search term"}, sessionRef{Unidentified: true}},
		{[]string{"--resume="}, sessionRef{}},
		{[]string{"--resume=term"}, sessionRef{Unidentified: true}},
		// Conflicting selectors name nothing headroom can stand behind.
		{[]string{"--continue", "--resume", sessA}, sessionRef{Unidentified: true}},
		// After a bare `--` everything is the vendor's positional text.
		{[]string{"--", "--resume", sessA}, sessionRef{}},
		{[]string{"-p", "--", "--continue"}, sessionRef{}},
		// A value that merely follows the flag is not thereby a session id.
		{[]string{"--session-id", "nope"}, sessionRef{}},
	}
	for _, c := range cases {
		if got := sessionIntent(c.args); got != c.want {
			t.Errorf("%v → %+v, want %+v", c.args, got, c.want)
		}
	}
}

// Degraded bookkeeping costs the launch its record and a line on stderr. It
// never refuses, and it never writes over what it could not read.
func TestDegradedBookkeepingNeverRefuses(t *testing.T) {
	t.Run("held lock", func(t *testing.T) {
		f := newAutoFixture(t, "a@x.com")
		f.setCurrent("auto\n")
		if err := rehome(f.st, sessA, "a@x.com", time.Now()); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(f.cfg.AccountsRoot, "state.json")
		before, _ := os.ReadFile(statePath)
		held, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN)

		got := f.launch()
		if got.code != 0 || !got.called || !strings.Contains(got.stderr, "placement not recorded") {
			t.Fatalf("under a held lock: %+v", got)
		}
		if recs := f.log(); len(recs) != 1 || recs[0].Recorded || recs[0].Problem == "" {
			t.Errorf("log = %+v", recs)
		}
		if after, _ := os.ReadFile(statePath); !bytes.Equal(before, after) {
			t.Error("state.json changed under a lock the launch never held")
		}
	})
	t.Run("newer schema", func(t *testing.T) {
		f := newAutoFixture(t, "a@x.com")
		f.setCurrent("auto\n")
		statePath := filepath.Join(f.cfg.AccountsRoot, "state.json")
		doc := `{"version":99,"sessions":{"` + sessA + `":{"account":"a@x.com","atMs":5}}}`
		writeJSON(t, statePath, doc)
		got := f.launch()
		if got.code != 0 || !got.called || !strings.Contains(got.stderr, "placement not recorded") {
			t.Fatalf("under a newer schema: %+v", got)
		}
		if after, _ := os.ReadFile(statePath); string(after) != doc {
			t.Error("a newer document was rewritten")
		}
	})
	t.Run("unreadable placements", func(t *testing.T) {
		f := newAutoFixture(t, "a@x.com")
		f.setCurrent("auto\n")
		statePath := filepath.Join(f.cfg.AccountsRoot, "state.json")
		writeJSON(t, statePath, `{"version":1,"placements":[1,2],"sessions":{"`+sessA+`":{"account":"a@x.com","atMs":5}}}`)
		got := f.launch()
		if got.code != 0 || !got.called || strings.Contains(got.stderr, "not recorded") {
			t.Fatalf("over an unreadable placements section: %+v", got)
		}
		snap := f.st.Load()
		if rec, _ := snap.Owner(sessA); rec.Account != "a@x.com" || rec.AtMS != 5 {
			t.Errorf("a re-home beside the damage changed: %+v", rec)
		}
		if len(snap.Placements().Recent) != 1 {
			t.Error("the record was not rebuilt")
		}
	})
	t.Run("unwritable log", func(t *testing.T) {
		f := newAutoFixture(t, "a@x.com")
		f.setCurrent("auto\n")
		if err := os.Mkdir(launchlog.Path(f.cfg.AccountsRoot), 0o755); err != nil {
			t.Fatal(err)
		}
		got := f.launch()
		if got.code != 0 || !got.called || !strings.Contains(got.stderr, "launch log not written") {
			t.Fatalf("with an unwritable log: %+v", got)
		}
	})
}

// The log is never an input: without it, with a torn last line, and with its
// lock held, a launch chooses the same account.
func TestTheLogIsInert(t *testing.T) {
	choose := func(t *testing.T, damage func(root string)) string {
		f := newAutoFixture(t, "a@x.com", "b@x.com")
		f.setCurrent("auto\n")
		f.observe("qiushi", 20, 10, time.Minute)
		f.observe("a@x.com", 0, 30, time.Minute)
		f.observe("b@x.com", 0, 20, time.Minute)
		if got := f.launch(); got.code != 0 {
			t.Fatal(got.stderr)
		}
		damage(f.cfg.AccountsRoot)
		start := time.Now()
		got := f.launch()
		if got.code != 0 || time.Since(start) > 2*time.Second {
			t.Fatalf("exit %d after %v: %s", got.code, time.Since(start), got.stderr)
		}
		return got.account(f.cfg)
	}
	want := choose(t, func(string) {})
	if got := choose(t, func(root string) { os.Remove(launchlog.Path(root)) }); got != want {
		t.Errorf("with the log deleted: %s, want %s", got, want)
	}
	if got := choose(t, func(root string) {
		f, _ := os.OpenFile(launchlog.Path(root), os.O_APPEND|os.O_WRONLY, 0o600)
		f.WriteString(`{"v":1,"at":"2026-`)
		f.Close()
	}); got != want {
		t.Errorf("with a torn last line: %s, want %s", got, want)
	}
	if got := choose(t, func(root string) {
		lock, _ := os.OpenFile(launchlog.Path(root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		t.Cleanup(func() { lock.Close() })
	}); got != want {
		t.Errorf("with the log's lock held: %s, want %s", got, want)
	}
}

// What the vendor stored is what the rule weighs: an account whose weekly room
// ends tomorrow takes the launch from one with less used and a week to run,
// and the line, the table and the log each say when that room ends.
func TestAWeekEndingSoonerTakesTheLaunch(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observeWeek("qiushi", 0, 5, 6*24*time.Hour, time.Minute)
	f.observeWeek("a@x.com", 0, 30, 20*time.Hour, time.Minute)

	table := captureStdout(t, func() { f.launch("--dry-run") })
	for _, want := range []string{"resets", "→ a@x.com", "20.0h"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
	got := f.launch()
	if got.code != 0 || !strings.Contains(got.stderr, "a@x.com · auto · 5h 0% · week 30%, resets in 20.0h") {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	recs, _, _ := launchlog.Read(f.cfg.AccountsRoot, 0)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	for _, c := range recs[0].Candidates {
		if c.Name == "a@x.com" && (c.Week.Kind != "weekly_all" || c.Week.Room != 50 || c.Week.LeftBasis != "reset" || c.Week.LeftS <= 19*3600) {
			t.Errorf("logged week = %+v", c.Week)
		}
	}
}

// An idle account's stored weekly reset has passed since its figures were
// taken. Claude Code renews that window every seven days, so it renews again
// in 155 h — sooner than a@x.com's 166 h — and the launch goes there, with the
// table saying the renewal is projected rather than reported.
func TestAPassedWeeklyResetIsProjectedOnItsSchedule(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observeWeek("qiushi", 0, 60, -13*time.Hour, 14*time.Hour)
	f.observeWeek("a@x.com", 0, 0, 166*time.Hour, time.Minute)

	table := captureStdout(t, func() { f.launch("--dry-run") })
	for _, want := range []string{"→ qiushi", "ended", "≈6.5d"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
	if got := f.launch(); got.code != 0 || !strings.Contains(got.stderr, "qiushi · auto") || strings.Contains(got.stderr, "resets in") {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	recs, _, _ := launchlog.Read(f.cfg.AccountsRoot, 0)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	for _, c := range recs[0].Candidates {
		if c.Name != "qiushi" {
			continue
		}
		if c.Week.LeftBasis != "projected" || c.Week.LeftS < 154*3600 || c.Week.LeftS > 156*3600 {
			t.Errorf("logged week = %+v", c.Week)
		}
		for _, l := range c.Limits {
			if !l.Session && l.PeriodS != 604800 {
				t.Errorf("weekly row %s logged without its schedule: %+v", l.Kind, l)
			}
		}
	}
}

func TestDryRunHasNoEffects(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 40, 40, time.Minute)
	f.observe("a@x.com", 0, 85, time.Minute)
	if got := f.launch(); got.code != 0 {
		t.Fatal(got.stderr)
	}
	files := []string{filepath.Join(f.cfg.AccountsRoot, "state.json"), launchlog.Path(f.cfg.AccountsRoot), f.cfg.CurrentFile()}
	read := func() [][]byte {
		var out [][]byte
		for _, p := range files {
			data, _ := os.ReadFile(p)
			out = append(out, data)
		}
		return out
	}
	before, refreshes := read(), *f.refreshes

	r, w, _ := os.Pipe()
	prev := os.Stdout
	os.Stdout = w
	got := f.launch("--dry-run")
	w.Close()
	os.Stdout = prev
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	table := string(buf[:n])

	if got.code != 0 || got.called {
		t.Fatalf("dry run: exit %d called %v", got.code, got.called)
	}
	for i, data := range read() {
		if !bytes.Equal(data, before[i]) {
			t.Errorf("%s changed", files[i])
		}
	}
	if *f.refreshes != refreshes {
		t.Error("a dry run started a refresh")
	}
	for _, want := range []string{"would start claude on qiushi", "→ qiushi", "near a limit (All models (7d) 85%)", "nothing was recorded"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
	// It previews whatever the launch would decide, forced included.
	if got := f.launch("--dry-run", "--remember"); got.code != 2 {
		t.Errorf("--dry-run --remember: exit %d, want 2", got.code)
	}
}

func TestOverrides(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	for _, name := range []string{"qiushi", "a@x.com", "b@x.com"} {
		f.observe(name, 0, 10, time.Minute)
	}

	// With nothing recorded there is no last account.
	if got := f.launch("--last"); got.code != 1 || got.called || !strings.Contains(got.stderr, "no launch is recorded") {
		t.Fatalf("--last with nothing recorded: %+v", got)
	}
	// The last account used is whichever launch came last, however decided.
	f.setCurrent("b@x.com\n")
	pinned := f.launch()
	if got := f.launch("--last"); got.account(f.cfg) != pinned.account(f.cfg) || !strings.Contains(got.stderr, "the last account used") {
		t.Fatalf("--last after a pinned launch: %+v", got)
	}
	f.launch("--account", "a@x.com")
	if got := f.launch("--last"); got.account(f.cfg) != "a@x.com" {
		t.Fatalf("--last after a named launch went to %s", got.account(f.cfg))
	}
	// --auto places one launch and leaves the pin alone.
	before, _ := os.ReadFile(f.cfg.CurrentFile())
	auto := f.launch("--auto")
	if auto.code != 0 || !strings.Contains(auto.stderr, "· auto ·") {
		t.Fatalf("--auto under a pin: %+v", auto)
	}
	if after, _ := os.ReadFile(f.cfg.CurrentFile()); !bytes.Equal(before, after) {
		t.Errorf("--auto changed .current: %q → %q", before, after)
	}
	if got := f.launch("--last"); got.account(f.cfg) != auto.account(f.cfg) {
		t.Fatalf("--last after an automatic launch went to %s, want %s", got.account(f.cfg), auto.account(f.cfg))
	}
	// A removed account is not launched on the strength of an old record.
	if err := os.RemoveAll(filepath.Join(f.cfg.AccountsRoot, auto.account(f.cfg))); err != nil {
		t.Fatal(err)
	}
	if auto.account(f.cfg) != "qiushi" {
		if got := f.launch("--last"); got.code != 1 || got.called || !strings.Contains(got.stderr, "no longer a discovered account") {
			t.Fatalf("--last naming a removed account: %+v", got)
		}
	}

	for _, args := range [][]string{{"--auto", "--last"}, {"--auto", "--account", "a@x.com"}, {"--last", "--account", "a@x.com"}, {"--last", "--remember"}} {
		if got := f.launch(args...); got.code != 2 || got.called {
			t.Errorf("%v: exit %d called %v, want a usage error", args, got.code, got.called)
		}
	}
}

func TestRememberRecordsTheMode(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("a@x.com\n")
	if got := f.launch("--auto", "--remember"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if data, _ := os.ReadFile(f.cfg.CurrentFile()); string(data) != "auto\n" {
		t.Fatalf(".current = %q, want auto", data)
	}
	if set := accounts.Discover(f.cfg); set.Routing().Mode != "auto" {
		t.Errorf("mode = %q", set.Routing().Mode)
	}
	if _, err := accounts.Discover(f.cfg).Select(""); err != accounts.ErrAuto {
		t.Errorf("Select(\"\") under auto = %v, want ErrAuto", err)
	}
	// Enter's scriptable spelling pins again.
	if got := f.launch("--account", "a@x.com", "--remember"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if set := accounts.Discover(f.cfg); set.Routing().Mode != "pinned" {
		t.Errorf("mode after pinning = %q", set.Routing().Mode)
	}

	// A failed exec leaves the recorded mode, and says so.
	prev := execVendor
	execVendor = func(string, string, []string, []string) error { return os.ErrPermission }
	t.Cleanup(func() { execVendor = prev })
	out := captureStderr(t, func() {
		if code := runLaunch(f.cfg, []string{"--auto", "--remember"}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
	if !strings.Contains(out, ".current remains auto") {
		t.Errorf("missing persistence explanation: %s", out)
	}
}

func TestResolveRefusesUnderAuto(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	out := captureStderr(t, func() {
		if code := runResolve(f.cfg, nil); code != 1 {
			t.Errorf("bare resolve under auto: exit %d, want 1", code)
		}
	})
	if !strings.Contains(out, "--dry-run") {
		t.Errorf("the refusal does not point to --dry-run: %s", out)
	}
	if code := runResolve(f.cfg, []string{"a@x.com"}); code != 0 {
		t.Errorf("a name still resolves: exit %d", code)
	}
}

func TestEveryAccountNearALimitSaysSo(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 10, 95, time.Minute)
	f.observe("a@x.com", 10, 84, time.Minute)
	got := f.launch()
	if got.account(f.cfg) != "a@x.com" || !strings.Contains(got.stderr, "every account is near a limit · All models (7d) 84%, resets in") {
		t.Fatalf("went to %s:\n%s", got.account(f.cfg), got.stderr)
	}
}

// Busy sessions are load: the registry's own word for a session, read from a
// verified-live claim, counts one step.
func TestBusySessionsAreLoad(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 0, 5, time.Minute)
	f.observe("a@x.com", 0, 50, time.Minute)
	started := time.Now().Add(-time.Hour)
	sdir := filepath.Join(f.cfg.PrimaryDir(), "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := func(pid int, status string) {
		writeJSON(t, filepath.Join(sdir, fmt.Sprintf("%d.json", pid)),
			fmt.Sprintf(`{"pid":%d,"sessionId":"s%d","startedAt":%d,"status":%q}`, pid, pid, started.UnixMilli(), status))
	}
	rec(1, "busy")
	rec(2, "idle")
	rec(3, "shell")
	rec(4, "busy") // its process is gone: not live, not load
	for _, pid := range []int{1, 2, 3} {
		f.live[pid] = started.Unix()
	}
	got := f.launch()
	if got.account(f.cfg) != "a@x.com" {
		t.Fatalf("the primary's busy session was not counted:\n%s", got.stderr)
	}
	for _, c := range f.log()[0].Candidates {
		if c.Name == "qiushi" && (c.Busy != 1 || c.Load != 1 || strings.Join(c.Statuses, ",") != "busy,idle,shell") {
			t.Errorf("primary counted as %+v", c)
		}
	}
}

// The round an automatic launch leaves behind is the ordinary one: each
// account the claim permits is asked once, the answer is recorded by a process
// that lives to record it, and a second round inside the quiet period asks
// nobody. No vendor health probe runs — nobody is watching.
func TestRefreshAsksThroughTheClaimAndRecords(t *testing.T) {
	var requests atomic.Int32
	srv := usageServer(t, &requests)
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.cfg = withUsageURL(f.cfg, srv.URL)
	f.st = state.Open(f.cfg)

	// No Keychain here: a stub that finds nothing sends the reader to the file.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	probed := filepath.Join(t.TempDir(), "probed")
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntouch "+probed+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	far := time.Now().Add(time.Hour).UnixMilli()
	for _, name := range []string{"a@x.com", "b@x.com"} {
		writeJSON(t, filepath.Join(f.cfg.AccountsRoot, name, ".credentials.json"),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, name, far, far))
	}

	if code := runRefresh([]config.Scope{f.cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests, want one per account that can be asked", got)
	}
	snap := f.st.Load()
	for _, name := range []string{"a@x.com", "b@x.com"} {
		if _, ok := snap.Observation(f.key(name), time.Now()); !ok {
			t.Errorf("%s: the answer was not recorded", name)
		}
	}
	if _, err := os.Stat(probed); err == nil {
		t.Error("the unattended round ran the vendor's health probe")
	}

	if code := runRefresh([]config.Scope{f.cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("a second round inside the quiet period made %d more request(s)", got-2)
	}
}

func TestLaunchesPrintsTheLog(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 20, 40, time.Minute)
	f.observe("a@x.com", 0, 10, time.Minute)
	f.launch()
	f.launch("--account", "qiushi")
	f.launch()

	var out bytes.Buffer
	if code := runLaunchesTo(&out, []config.Scope{f.cfg}, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "a@x.com") || !strings.Contains(lines[0], "auto/least-load") || !strings.Contains(lines[0], "next qiushi") {
		t.Errorf("first line: %s", lines[0])
	}
	if !strings.Contains(lines[1], "qiushi") || !strings.Contains(lines[1], "named") || strings.Contains(lines[1], "named/named") {
		t.Errorf("second line: %s", lines[1])
	}

	out.Reset()
	if code := runLaunchesTo(&out, []config.Scope{f.cfg}, []string{"-n", "1", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rec launchlog.Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil || rec.Mode != "auto" || len(rec.Candidates) != 2 {
		t.Errorf("--json -n 1: %v\n%s", err, out.String())
	}
	for _, args := range [][]string{{"-n"}, {"-n", "many"}, {"--nope"}} {
		if code := runLaunchesTo(&out, []config.Scope{f.cfg}, args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	out.Reset()
	empty := newAutoFixture(t)
	if code := runLaunchesTo(&out, []config.Scope{empty.cfg}, nil); code != 0 || !strings.Contains(out.String(), "no launches recorded") {
		t.Errorf("empty log: exit %d, %q", code, out.String())
	}
}

// rehome moves a session to an account the way the picker's override does: a
// launch that exists to move it.
func rehome(st *state.Store, id, account string, at time.Time) error {
	_, err := st.Place(state.Launch{
		Candidates: []placement.Candidate{{Name: account, Key: "test:" + account}},
		Intent:     placement.Intent{Kind: placement.Forced, Account: account, Reason: "test"},
		Session:    id, MustReHome: true, Now: at,
	})
	return err
}

func readLog(t *testing.T, scope config.Scope) ([]launchlog.Record, int, error) {
	t.Helper()
	recs, skipped, err := launchlog.Read(scope.AccountsRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	return recs, skipped, err
}
