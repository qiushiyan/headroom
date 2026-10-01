package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// A resume that is refused starts nothing, so it must leave nothing behind: no
// load for the next launch to count, no line in the log, no "last account".
func TestARefusedResumeLeavesNoPlacement(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	// A sessions section that will not validate: re-homes cannot be written.
	writeJSON(t, filepath.Join(f.cfg.AccountsRoot, "state.json"), `{"version":1,"sessions":{"hollow":{"account":"","atMs":5}}}`)
	proj := filepath.Join(f.cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	set := accounts.Discover(f.cfg)
	ui := &resumeUI{sessionActions: sessionActions{beforeLaunch: func() {}, cfg: f.cfg, st: f.st, set: set, auto: true}}
	s := &sessions.Session{ID: sessA, CWD: proj, DirOK: true, Owner: "a@x.com", OwnerState: sessions.OwnerHistory}
	ui.listing.Sessions = []*sessions.Session{s}
	ui.rows = ui.listing.Sessions

	_, _, _, called := capturedSessionsExec(t)
	if done, _ := ui.commitResume(true); done || *called {
		t.Fatalf("x with re-homes unwritable: done %v, exec called %v", done, *called)
	}
	if n := len(f.st.Load().Placements().Recent); n != 0 {
		t.Errorf("a refused resume left %d placement(s) as load", n)
	}
	if recs := f.log(); len(recs) != 0 {
		t.Errorf("a refused resume was logged as a launch: %+v", recs)
	}

	// The same picker, the next attempt: an ordinary resume on the owner is
	// the one launch that happened, counted once.
	_, _, env, called := capturedSessionsExec(t)
	if done, code := ui.commitResume(false); !done || code != 0 || !*called {
		t.Fatalf("enter after the refusal: (%v, %d) — %s", done, code, ui.message)
	}
	if dir, _ := envValue(*env, "CLAUDE_CONFIG_DIR"); filepath.Base(dir) != "a@x.com" {
		t.Errorf("resumed on %q", dir)
	}
	if n := len(f.st.Load().Placements().Recent); n != 1 {
		t.Errorf("%d placements after one committed resume", n)
	}
	if recs := f.log(); len(recs) != 1 || recs[0].Chosen != "a@x.com" {
		t.Errorf("log = %+v", recs)
	}
}

// loginFixture is a boardFixture whose accounts are judged by the real readers:
// credentials from each dir's .credentials.json (the stub Keychain finds
// nothing) and health from a stub `claude auth status` that says logged in.
func loginFixture(t *testing.T, usageURL string, extras ...string) autoFixture {
	t.Helper()
	f := newAutoFixture(t, extras...)
	f.cfg = withUsageURL(f.cfg, usageURL)
	f.st = state.Open(f.cfg)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"security": "#!/bin/sh\nexit 44\n",
		"claude":   "#!/bin/sh\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true}'; fi\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Judged by the launch path's own reader, not a stand-in for it.
	placementCreds = productionCreds
	return f
}

func (f autoFixture) credential(name string, accessExpires, refreshExpires time.Time) {
	f.t.Helper()
	dir := filepath.Join(f.cfg.AccountsRoot, name)
	if name == "qiushi" {
		dir = f.cfg.PrimaryDir()
	}
	writeJSON(f.t, filepath.Join(dir, ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
			name, accessExpires.UnixMilli(), refreshExpires.UnixMilli()))
}

// One placement contract, one owner: what the board marks and what a launch
// takes are computed from the same evidence. The vendor's health probe may say
// "logged in" over a refresh token that has demonstrably expired; a launch
// goes by the credential, so the mark must too.
func TestTheBoardAndALaunchJudgeAccountsByTheSameEvidence(t *testing.T) {
	f := loginFixture(t, "http://127.0.0.1:1/usage", "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	now := time.Now()
	f.credential("a@x.com", now.Add(-time.Hour), now.Add(-time.Hour)) // refresh token expired
	f.credential("b@x.com", now.Add(-time.Hour), now.Add(30*24*time.Hour))
	f.observe("qiushi", 60, 60, time.Minute)
	f.observe("a@x.com", 0, 5, time.Minute)
	f.observe("b@x.com", 20, 30, time.Minute)

	board := fetchBoards([]config.Scope{f.cfg})[0]
	_, next := marks(board.list)
	got := f.launch()
	if got.code != 0 {
		t.Fatal(got.stderr)
	}
	if len(next) != 1 || next[0] != got.account(f.cfg) {
		t.Fatalf("the board marked %v and a launch with the same inputs took %s", next, got.account(f.cfg))
	}
	if got.account(f.cfg) != "b@x.com" {
		t.Fatalf("launched on %s, whose login has expired", got.account(f.cfg))
	}
}

// The document says one thing about routing: mode, current and the per-account
// flags come from one read of `.current`, however long the round takes and
// whatever is written meanwhile.
func TestTheDocumentCarriesOneRoutingSnapshot(t *testing.T) {
	var current string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The mode changes while the round is in flight.
		os.WriteFile(current, []byte("auto\n"), 0o644)
		w.Write([]byte(`{"limits":[{"kind":"session","percent":5}]}`))
	}))
	t.Cleanup(srv.Close)
	f := loginFixture(t, srv.URL, "a@x.com")
	current = f.cfg.CurrentFile()
	f.setCurrent("a@x.com\n")
	f.credential("a@x.com", time.Now().Add(time.Hour), time.Now().Add(30*24*time.Hour))

	data, err := jsonDocument(fetchBoards([]config.Scope{f.cfg}), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := decode5(t, data)
	marked := 0
	for _, a := range d.Accounts {
		if a.Current {
			marked++
		}
	}
	mode, cur := d.Mode["claude"], d.Current["claude"]
	pinned := mode == "pinned" && cur == "a@x.com" && marked == 1
	auto := mode == "auto" && cur == "" && marked == 0
	if !pinned && !auto {
		t.Fatalf("the document contradicts itself: mode %q, current %q, %d account(s) marked current", mode, cur, marked)
	}
}

// A figure the rule counted as a bound, or as an ended window, is said as one.
// "5h 0%" over a window that was at 97% a minute ago and has since reset is a
// measured-looking number nobody measured.
func TestTheLaunchLineSaysWhatAFigureRestsOn(t *testing.T) {
	now := time.Unix(1_790_848_000, 0)
	limit := func(session bool, kind, label string, percent int, reset time.Duration) placement.Limit {
		return placement.Limit{Kind: kind, Label: label, Percent: percent, ResetAt: now.Add(reset).Unix(), Session: session}
	}
	choose := func(c placement.Candidate) string {
		d := placement.Choose([]placement.Candidate{c}, placement.Ledger{}, placement.Intent{}, now)
		return launchLine(d, true, sessionRef{}, "", now)
	}

	ended := choose(placement.Candidate{Name: "b", Key: "b", ObservedAt: now.Add(-time.Minute).Unix(), Limits: []placement.Limit{
		limit(true, "session", "5h session", 97, -30*time.Second),
		limit(false, "weekly_all", "All models (7d)", 12, 48*time.Hour),
	}})
	if strings.Contains(ended, "5h 0%") || !strings.Contains(ended, "5h window ended") {
		t.Errorf("an ended window: %q", ended)
	}

	stale := choose(placement.Candidate{Name: "b", Key: "b", ObservedAt: now.Add(-40 * time.Hour).Unix(), Limits: []placement.Limit{
		limit(true, "session", "5h session", 12, time.Hour),
		limit(false, "weekly_all", "All models (7d)", 30, 48*time.Hour),
	}})
	if !strings.Contains(stale, "5h ≥12%") || !strings.Contains(stale, "week ≥30%") || !strings.Contains(stale, "figures 1d old") {
		t.Errorf("stale figures are lower bounds: %q", stale)
	}

	bad := placement.Candidate{Name: "b", Key: "b", ObservedAt: now.Add(-time.Minute).Unix(), Limits: []placement.Limit{
		{Kind: "session", Label: "5h session", Bad: true, Session: true},
	}}
	if line := choose(bad); strings.Contains(line, "100%") || !strings.Contains(line, "5h ?%") {
		t.Errorf("an unparseable percent: %q", line)
	}

	// Every account near a limit, on old figures: the age is still said.
	near := choose(placement.Candidate{Name: "b", Key: "b", ObservedAt: now.Add(-40 * time.Hour).Unix(), Limits: []placement.Limit{
		limit(true, "session", "5h session", 10, time.Hour),
		limit(false, "weekly_all", "All models (7d)", 91, 48*time.Hour),
	}})
	if !strings.Contains(near, "every account is near a limit") || !strings.Contains(near, "≥91%") || !strings.Contains(near, "figures 1d old") {
		t.Errorf("near a limit on stale figures: %q", near)
	}
}

// productionCreds is the launch path's own credential reader, captured before
// any fixture replaces it.
var productionCreds = placementCreds

// The launch path reads credentials through the reader the board uses: the
// Keychain first, then the account's own .credentials.json — the primary's
// included. An expired login found only in the primary's file must keep an
// automatic launch off the primary.
func TestPlacementReadsThePrimarysFileBackedCredential(t *testing.T) {
	f := loginFixture(t, "http://127.0.0.1:1/usage", "a@x.com")
	f.setCurrent("auto\n")
	now := time.Now()
	f.credential("qiushi", now.Add(-time.Hour), now.Add(-time.Hour)) // refresh token expired
	f.credential("a@x.com", now.Add(time.Hour), now.Add(30*24*time.Hour))
	f.observe("qiushi", 0, 0, time.Minute)
	f.observe("a@x.com", 60, 60, time.Minute)

	got := f.launch()
	if got.code != 0 || got.account(f.cfg) != "a@x.com" {
		t.Fatalf("launched on %s:\n%s", got.account(f.cfg), got.stderr)
	}
	for _, c := range f.log()[0].Candidates {
		if c.Name == "qiushi" && c.Excluded != "login expired" {
			t.Errorf("the primary's file-backed credential was not read: excluded %q", c.Excluded)
		}
	}
}

// The refresh an automatic launch leaves behind, through the real binary: the
// launch returns while the usage endpoint has not answered, and the answer is
// recorded afterwards by a process that outlived it.
func TestTheDetachedRefreshOutlivesTheLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "headroom")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/headroom").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	asked := make(chan string, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.Header.Get("Authorization")
		<-release
		w.Write([]byte(`{"limits":[{"kind":"session","group":"session","percent":7}]}`))
	}))
	t.Cleanup(srv.Close)
	// Closed before the server, so a stalled handler cannot hold Close up.
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})

	home := t.TempDir()
	stubs := t.TempDir()
	for name, body := range map[string]string{"claude": "#!/bin/sh\nexit 0\n", "security": "#!/bin/sh\nexit 44\n"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.AccountsRoot, "a@x.com")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u-a"}}`)
	far := time.Now().Add(time.Hour).UnixMilli()
	writeJSON(t, filepath.Join(dir, ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-a","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, far, far))
	if err := os.Symlink(cfg.StoreDir(), filepath.Join(dir, "projects")); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.CurrentFile(), "auto\n")

	cmd := exec.Command(bin, "launch", "--", "-p", "hello")
	cmd.Env = append(os.Environ(),
		"HEADROOM_HOME="+home, "HEADROOM_ACCOUNTS_ROOT="+cfg.AccountsRoot, "HEADROOM_PRIMARY_NAME=primary",
		"HEADROOM_USAGE_URL="+srv.URL, "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "a@x.com · auto") {
		t.Fatalf("launch output:\n%s", out)
	}
	t.Logf("launch returned in %v with the endpoint still silent", time.Since(start))

	// The launcher is gone. The request arrives from the process it left
	// behind, carrying the account's token…
	select {
	case auth := <-asked:
		if auth != "Bearer tok-a" {
			t.Errorf("the refresh asked with %q", auth)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no usage request arrived: the launch left no refresh behind it")
	}
	st := state.Open(cfg)
	key := state.Key{UUID: "u-a", Name: "a@x.com"}
	if _, ok := st.Load().Observation(key, time.Now()); ok {
		t.Fatal("an observation exists before the endpoint answered")
	}
	// …and its answer is recorded once the endpoint gives one.
	close(release)
	released = true
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := st.Load().Observation(key, time.Now()); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the endpoint answered and nothing recorded it: the refresh did not outlive the launch")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// What a launch owes the next one belongs to the launch, not to the surface
// that started it: the picker's launch says where it went and what bookkeeping
// it missed, and one the rule placed leaves a refresh behind it, exactly as
// `headroom launch` does.
func TestAPickerLaunchIsAnnouncedAndLeavesARefresh(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 50, 50, 3*time.Minute)
	f.observe("a@x.com", 0, 5, 3*time.Minute)
	f.observe("b@x.com", 0, 30, 3*time.Minute)
	// A log that cannot be appended to: the launch still starts, and says so.
	if err := os.Mkdir(launchlog.Path(f.cfg.AccountsRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(f.cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	set := accounts.Discover(f.cfg)
	resume := func(owner string, state sessions.OwnerState) string {
		t.Helper()
		ui := &resumeUI{sessionActions: sessionActions{beforeLaunch: func() {}, cfg: f.cfg, st: f.st, set: set, auto: true}}
		ui.listing.Sessions = []*sessions.Session{{ID: sessA, CWD: proj, DirOK: true, Owner: owner, OwnerState: state}}
		ui.rows = ui.listing.Sessions
		_, _, _, called := capturedSessionsExec(t)
		var done bool
		stderr := captureStderr(t, func() { done, _ = ui.commitResume(false) })
		if !done || !*called {
			t.Fatalf("resume: done %v, exec called %v — %s", done, *called, ui.message)
		}
		return stderr
	}

	stderr := resume("", sessions.OwnerNone)
	for _, want := range []string{"headroom sessions: a@x.com · auto", "headroom sessions: launch log not written"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the picker's launch did not say %q:\n%s", want, stderr)
		}
	}
	if *f.refreshes != 1 {
		t.Errorf("%d refreshes started by a resume the rule placed, want 1", *f.refreshes)
	}

	// A resume that follows its owner chose nothing: no refresh for it.
	if stderr := resume("b@x.com", sessions.OwnerHistory); !strings.Contains(stderr, "headroom sessions: b@x.com") {
		t.Errorf("a resume on the owner was not announced:\n%s", stderr)
	}
	if *f.refreshes != 1 {
		t.Errorf("%d refreshes after a resume on the owner, want still 1", *f.refreshes)
	}
}

// A launch that records where a session goes also clears the records of
// sessions whose transcripts are gone — whichever surface it came from. Left
// to the picker's `x` alone, an agent that names a new session id every run
// would grow the file for as long as nobody pressed `x`.
func TestALaunchThatRecordsASessionSweepsTheGoneOnes(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("a@x.com", 0, 5, time.Minute)
	const (
		gone  = "aaaaaaaa-1111-4111-8111-111111111111"
		kept  = "bbbbbbbb-2222-4222-8222-222222222222"
		young = "cccccccc-3333-4333-8333-333333333333"
	)
	old := time.Now().Add(-time.Hour)
	for id, at := range map[string]time.Time{gone: old, kept: old, young: time.Now().Add(-time.Minute)} {
		if err := rehome(f.st, id, "b@x.com", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.cfg.StoreDir(), "-proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(f.cfg.StoreDir(), "-proj", kept+".jsonl"), "{}\n")

	if got := f.launch("--", "-p", "--session-id", sessA); got.code != 0 {
		t.Fatalf("launch: %s", got.stderr)
	}
	snap := f.st.Load()
	if _, ok := snap.Owner(sessA); !ok {
		t.Fatal("the launch did not record its own session")
	}
	if _, ok := snap.Owner(gone); ok {
		t.Error("a re-home whose transcript is gone survived a launch that wrote one")
	}
	if _, ok := snap.Owner(kept); !ok {
		t.Error("a re-home whose transcript exists was swept")
	}
	if _, ok := snap.Owner(young); !ok {
		t.Error("a re-home too young to have a transcript yet was swept")
	}
}
