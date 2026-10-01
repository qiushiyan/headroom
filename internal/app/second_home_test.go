package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

// twoHomes is the owner's home and a second home on the same machine — an
// automated pipeline's — each holding its own logins of the same
// subscriptions. The second home spends against the owner's ledger and spells
// its primary out, as a home whose HOME is not the user's login home does.
// The launch path's edges are replaced as in autoFixture: no Keychain, no
// process table, no detached child.
type twoHomes struct {
	t              *testing.T
	owner, steward config.Scope
	live           map[int]int64
	refreshes      *int
}

// The subscriptions both homes hold, by email; the owner's primary is a
// fourth, and the steward's primary is logged into yan@x.com.
var shared = []string{"a@x.com", "b@x.com", "yan@x.com"}

func newTwoHomes(t *testing.T) *twoHomes {
	t.Helper()
	base := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")

	owner := claudeScope(filepath.Join(base, "qiushiyan"), "qiushi")
	steward := config.ForHome(filepath.Join(base, ".steward-home")).Claude
	steward.PrimaryExplicit = true
	steward.LedgerRoot = owner.AccountsRoot

	login := func(meta, email string) {
		if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, meta, fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, email, email))
	}
	for _, s := range []config.Scope{owner, steward} {
		if err := os.MkdirAll(s.StoreDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, email := range shared {
			dir := filepath.Join(s.AccountsRoot, email)
			login(filepath.Join(dir, ".claude.json"), email)
			if err := os.Symlink(s.StoreDir(), filepath.Join(dir, "projects")); err != nil {
				t.Fatal(err)
			}
		}
	}
	login(owner.PrimaryMeta(), "qiushi@x.com")
	login(steward.PrimaryMeta(), "yan@x.com")

	f := &twoHomes{t: t, owner: owner, steward: steward, live: map[int]int64{}, refreshes: new(int)}
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

	for _, s := range []config.Scope{owner, steward} {
		if err := state.Open(s).Register(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// observe stores a usage response for a subscription in the shared ledger, as
// taken a minute ago. Either home's store would do: they are one ledger.
func (f *twoHomes) observe(email string, session, weekly int) {
	f.t.Helper()
	at := time.Now().Add(-time.Minute)
	reset := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	body := fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":%d,"resets_at":%q},`+
		`{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":%q}]}`, session, reset(2*time.Hour), weekly, reset(72*time.Hour))
	st := state.Open(f.steward)
	k := state.Key{UUID: "u-" + email, Name: email}
	dec, err := st.Claim([]state.Key{k}, at)
	if err != nil || !dec[0].Permit {
		f.t.Fatalf("claim %s: %+v %v", email, dec, err)
	}
	if _, err := st.Complete(k, dec[0].Generation, state.OutcomeStored, []byte(body), at); err != nil {
		f.t.Fatal(err)
	}
}

// busy registers a session the vendor reports as working, in one account dir.
func (f *twoHomes) busy(dir string, pid int) {
	f.t.Helper()
	started := time.Now().Add(-time.Hour)
	sdir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	writeJSON(f.t, filepath.Join(sdir, fmt.Sprintf("%d.json", pid)),
		fmt.Sprintf(`{"pid":%d,"sessionId":"s%d","startedAt":%d,"status":"busy"}`, pid, pid, started.UnixMilli()))
	f.live[pid] = started.Unix()
}

func (f *twoHomes) launch(cfg config.Scope, args ...string) launched {
	f.t.Helper()
	var out launched
	prev := execVendor
	execVendor = func(path, binary string, vendorArgs, environ []string) error {
		out.called, out.args, out.env = true, vendorArgs, environ
		return nil
	}
	defer func() { execVendor = prev }()
	out.stderr = captureStderr(f.t, func() { out.code = runLaunch(cfg, args) })
	return out
}

func (f *twoHomes) log(cfg config.Scope) []launchlog.Record {
	f.t.Helper()
	recs, _, err := launchlog.Read(cfg.AccountsRoot, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return recs
}

// configDirs is every CLAUDE_CONFIG_DIR a child environment carries.
func configDirs(env []string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			out = append(out, v)
		}
	}
	return out
}

func loggedCandidate(rec launchlog.Record, name string) launchlog.Candidate {
	for _, c := range rec.Candidates {
		if c.Name == name {
			return c
		}
	}
	return launchlog.Candidate{}
}

// A launch in the second home counts the sessions running in the first home
// on the same subscriptions, and says how much of its account's load is
// theirs.
func TestASecondHomeCountsTheFirstHomesBusySessions(t *testing.T) {
	f := newTwoHomes(t)
	f.observe("a@x.com", 0, 10)
	f.observe("b@x.com", 0, 20)
	f.observe("yan@x.com", 0, 30)
	ownerDir := func(email string) string { return filepath.Join(f.owner.AccountsRoot, email) }
	f.busy(ownerDir("a@x.com"), 501)
	f.busy(ownerDir("a@x.com"), 502)
	f.busy(ownerDir("b@x.com"), 503)
	f.busy(ownerDir("yan@x.com"), 504)

	got := f.launch(f.steward, "--auto", "--", "-p", "hi")
	if got.code != 0 || !got.called {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	// Alone, every account is load 0 and a has the most weekly room. With the
	// owner's sessions counted, a is load 2 and b wins on weekly room over
	// yan, whose subscription the owner is also using.
	if dirs := configDirs(got.env); len(dirs) != 1 || dirs[0] != filepath.Join(f.steward.AccountsRoot, "b@x.com") {
		t.Fatalf("launched on %v:\n%s", dirs, got.stderr)
	}
	if !strings.Contains(got.stderr, "load 1 (1 from qiushiyan)") {
		t.Errorf("the launch line does not say whose load it counted:\n%s", got.stderr)
	}
	rec := f.log(f.steward)[0]
	if c := loggedCandidate(rec, "a@x.com"); c.Busy != 2 || len(c.Homes) != 1 || c.Homes[0].Home != f.owner.AccountsRoot {
		t.Errorf("a@x.com counted as %+v", c)
	}
	// The second home's primary is the same subscription as its yan@x.com dir:
	// both carry the owner's session on it.
	if c := loggedCandidate(rec, "yan"); c.Busy != 1 {
		t.Errorf("the second home's primary counted as %+v", c)
	}
}

// The reverse: a launch in the first home counts the launches the second home
// just made and the sessions it is running.
func TestTheFirstHomeCountsTheSecondHomesLaunchesAndSessions(t *testing.T) {
	f := newTwoHomes(t)
	f.observe("a@x.com", 0, 10)
	f.observe("b@x.com", 0, 20)
	f.observe("yan@x.com", 0, 30)
	f.observe("qiushi@x.com", 0, 90) // set aside: near its weekly limit

	first := f.launch(f.steward, "--auto", "--", "-p", "hi")
	if dirs := configDirs(first.env); len(dirs) != 1 || filepath.Base(dirs[0]) != "a@x.com" {
		t.Fatalf("the second home's launch went to %v", dirs)
	}
	f.busy(filepath.Join(f.steward.AccountsRoot, "b@x.com"), 601)

	got := f.launch(f.owner, "--auto")
	if dirs := configDirs(got.env); len(dirs) != 1 || dirs[0] != filepath.Join(f.owner.AccountsRoot, "yan@x.com") {
		t.Fatalf("the owner's launch went to %v — a carries the second home's launch and b its session:\n%s", dirs, got.stderr)
	}
	rec := f.log(f.owner)[0]
	if c := loggedCandidate(rec, "a@x.com"); c.Pending != 1 || len(c.Homes) != 1 || c.Homes[0].Home != f.steward.AccountsRoot || c.Homes[0].Pending != 1 {
		t.Errorf("a@x.com counted as %+v", c)
	}
	if c := loggedCandidate(rec, "b@x.com"); c.Busy != 1 || len(c.Homes) != 1 || c.Homes[0].Busy != 1 {
		t.Errorf("b@x.com counted as %+v", c)
	}
}

// One request per subscription per spacing, whichever home asks first: the
// claim is the subscription's, and so is the answer the other home replays.
func TestEachSubscriptionIsAskedOncePerSpacingAcrossHomes(t *testing.T) {
	f := newTwoHomes(t)
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Write([]byte(`{"limits":[{"kind":"session","group":"session","percent":5}]}`))
	}))
	t.Cleanup(srv.Close)
	f.owner, f.steward = withUsageURL(f.owner, srv.URL), withUsageURL(f.steward, srv.URL)

	// File-backed logins in every dir of both homes, each with its own token;
	// no Keychain answers.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	far := time.Now().Add(time.Hour).UnixMilli()
	cred := func(dir, token string) {
		writeJSON(t, filepath.Join(dir, ".credentials.json"),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"refreshTokenExpiresAt":%d}}`, token, far, far))
	}
	for _, home := range []struct {
		name  string
		scope config.Scope
	}{{"owner", f.owner}, {"steward", f.steward}} {
		cred(home.scope.PrimaryDir(), home.name+"-primary")
		for _, email := range shared {
			cred(filepath.Join(home.scope.AccountsRoot, email), home.name+"-"+email)
		}
	}

	if code := runRefresh([]config.Scope{f.steward}); code != 0 {
		t.Fatalf("steward refresh: exit %d", code)
	}
	if code := runRefresh([]config.Scope{f.owner}); code != 0 {
		t.Fatalf("owner refresh: exit %d", code)
	}
	// Three subscriptions asked once by the second home, with its own logins;
	// the owner's round asks only about the one subscription it alone holds.
	total, owners := 0, 0
	for token, n := range asked {
		total += n
		if n != 1 {
			t.Errorf("%s asked %d times", token, n)
		}
		if strings.HasPrefix(token, "Bearer owner-") {
			owners++
		}
	}
	if total != 4 || owners != 1 || asked["Bearer owner-primary"] != 1 {
		t.Fatalf("requests = %v, want the three shared subscriptions once (by whichever home asked first) and the owner's own once", asked)
	}
	ownerSnap := state.Open(f.owner).Load()
	for _, email := range shared {
		if _, ok := ownerSnap.Observation(state.Key{UUID: "u-" + email}, time.Now()); !ok {
			t.Errorf("the owner's home has no figures for %s, which the second home bought", email)
		}
	}
}

// A launch runs on its own home's dirs and nothing else, whatever it inherits
// and however its account is decided — and the second home's primary is named
// by its dir, never by the variable's absence.
func TestALaunchNeverResolvesTheOtherHomesDirs(t *testing.T) {
	f := newTwoHomes(t)
	for _, email := range append(shared, "qiushi@x.com") {
		f.observe(email, 0, 10)
	}
	under := func(dir, root string) bool { return strings.HasPrefix(dir, root+string(os.PathSeparator)) }

	stewardHome, ownerHome := f.steward.Home, f.owner.Home
	for i := range 5 {
		got := f.launch(f.steward, "--auto", "--", "-p", fmt.Sprint(i))
		if dirs := configDirs(got.env); got.code != 0 || len(dirs) != 1 || !under(dirs[0], stewardHome) {
			t.Fatalf("second home's launch %d: exit %d, dirs %v\n%s", i, got.code, dirs, got.stderr)
		}
	}
	if got := f.launch(f.steward, "--account", "yan"); len(configDirs(got.env)) != 1 || configDirs(got.env)[0] != f.steward.PrimaryDir() {
		t.Errorf("the second home's primary launched as %v — it must be spelled out, never absent", configDirs(got.env))
	}
	if got := f.launch(f.steward, "--account", "qiushi"); got.code == 0 || got.called {
		t.Error("the second home launched the owner's primary by name")
	}

	// An inherited value naming the other home's dir is stripped and said.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.owner.AccountsRoot, "a@x.com"))
	got := f.launch(f.steward, "--auto")
	if dirs := configDirs(got.env); len(dirs) != 1 || !under(dirs[0], stewardHome) {
		t.Fatalf("an inherited owner dir survived: %v", dirs)
	}
	if !strings.Contains(got.stderr, "ignoring inherited CLAUDE_CONFIG_DIR="+filepath.Join(f.owner.AccountsRoot, "a@x.com")) {
		t.Errorf("the neutralized owner dir was not said:\n%s", got.stderr)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", f.steward.PrimaryDir())
	for i := range 5 {
		got := f.launch(f.owner, "--auto", "--", "-p", fmt.Sprint(i))
		for _, dir := range configDirs(got.env) {
			if !under(dir, ownerHome) {
				t.Fatalf("owner's launch %d ran on %s", i, dir)
			}
		}
	}
	if got := f.launch(f.owner, "--account", "qiushi"); got.code != 0 || len(configDirs(got.env)) != 0 {
		t.Errorf("the owner's primary is selected by absence: %v", configDirs(got.env))
	}
}

// A home's sessions are its own: its picker lists only its store, its re-homes
// live in its own file, and a session it started resumes where it started.
func TestEachHomesSessionsStayInItsHome(t *testing.T) {
	f := newTwoHomes(t)
	for _, email := range append(shared, "qiushi@x.com") {
		f.observe(email, 0, 10)
	}
	const stewardSession = "77777777-7777-4777-8777-777777777777"
	const ownerSession = "88888888-8888-4888-8888-888888888888"
	turn := []string{"--auto", "--", "-p", "--output-format", "stream-json", "--verbose", "--model", "claude-opus-5-5"}

	first := f.launch(f.steward, append(slices.Clone(turn), "--session-id", stewardSession)...)
	if first.code != 0 {
		t.Fatalf("first turn: %s", first.stderr)
	}
	stewardSt, ownerSt := state.Open(f.steward), state.Open(f.owner)
	if _, ok := stewardSt.Load().Owner(stewardSession); !ok {
		t.Fatal("the second home did not record where its new session went")
	}
	if _, ok := ownerSt.Load().Owner(stewardSession); ok {
		t.Error("the second home's session is routed in the owner's file")
	}

	transcript := func(s config.Scope, id, cwd string) {
		dir := filepath.Join(s.StoreDir(), strings.NewReplacer("/", "-", ".", "-").Replace(cwd))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, id+".jsonl"), fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q}`+"\n", cwd, id))
	}
	transcript(f.steward, stewardSession, f.steward.Home)
	transcript(f.owner, ownerSession, f.owner.Home)

	// The owner's launches sweep the owner's records against the owner's store.
	if got := f.launch(f.owner, "--auto", "--", "-p", "--session-id", "99999999-9999-4999-8999-999999999999"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if _, ok := stewardSt.Load().Owner(stewardSession); !ok {
		t.Error("the owner's launch swept the second home's record")
	}

	ids := func(s config.Scope) []string {
		listing, _, _, _ := collectSessions(s, state.Open(s))
		var out []string
		for _, sess := range listing.Sessions {
			out = append(out, sess.ID)
		}
		return out
	}
	if got := ids(f.owner); !slices.Equal(got, []string{ownerSession}) {
		t.Errorf("the owner's picker lists %v", got)
	}
	if got := ids(f.steward); !slices.Equal(got, []string{stewardSession}) {
		t.Errorf("the second home's picker lists %v", got)
	}

	// The next turn names the session and continues on the account it
	// started on, which still has room.
	next := f.launch(f.steward, append(slices.Clone(turn), "--resume", stewardSession)...)
	if a, b := configDirs(first.env), configDirs(next.env); len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		t.Fatalf("the session moved from %v to %v", a, b)
	}
	if !strings.Contains(next.stderr, "this session's account") {
		t.Errorf("the resume did not follow its session:\n%s", next.stderr)
	}
}

// The owner's board and --json count the second home's sessions and launches,
// and say whose they are.
func TestTheBoardShowsTheOtherHomesLoad(t *testing.T) {
	f := newTwoHomes(t)
	for _, email := range append(shared, "qiushi@x.com") {
		f.observe(email, 0, 10)
	}
	if got := f.launch(f.steward, "--account", "a@x.com"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	f.busy(filepath.Join(f.steward.AccountsRoot, "b@x.com"), 701)
	f.busy(filepath.Join(f.owner.AccountsRoot, "b@x.com"), 702)

	st := state.Open(f.owner)
	set := accounts.Discover(f.owner)
	now := time.Now()
	facts, routing := accountstate.Assemble(set, st.Load(), now)
	list := accountList(facts)
	markPlacement(set, list, routing.Mode, st, now)

	view := func(name string) accountstate.Facts {
		for _, d := range list {
			if d.Acct.Name == name {
				return d.View
			}
		}
		t.Fatalf("no %s", name)
		return accountstate.Facts{}
	}
	if l := view("a@x.com").Load; l == nil || l.Launched != 1 || len(l.Elsewhere()) != 1 || l.Elsewhere()[0].Label != "steward-home" {
		t.Fatalf("a@x.com load = %+v", l)
	}
	if l := view("qiushi").Load; l == nil || l.Busy+l.Launched != 0 {
		t.Errorf("an idle account's load = %+v", l)
	}

	p := render.NewPalette(false)
	blocks := strings.Join(slices.Concat(p.Board(views(list), now.Unix(), render.LayoutBlocks, 0).Groups...), "\n")
	for _, want := range []string{"sessions: 1 launched (steward-home: 1 launched)", "sessions: 2 busy (steward-home: 1 busy)"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("the board lacks %q:\n%s", want, blocks)
		}
	}
	compact := strings.Join(slices.Concat(p.Board(views(list), now.Unix(), render.LayoutCompact, 0).Groups...), "\n")
	if !strings.Contains(compact, "2 busy (steward-home: 1 busy)") {
		t.Errorf("the compact board lacks the load clause:\n%s", compact)
	}

	data, err := jsonDocument([]vendorBoard{{scope: f.owner, set: set, st: st, list: list, mode: routing.Mode}}, now)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []struct {
			Name string
			Load *struct {
				Busy, Launched int
				Homes          []struct {
					Label          string
					This           bool
					Busy, Launched int
				}
			}
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.Name != "b@x.com" {
			continue
		}
		if a.Load == nil || a.Load.Busy != 2 || len(a.Load.Homes) != 2 {
			t.Fatalf("b@x.com load = %+v", a.Load)
		}
		for _, h := range a.Load.Homes {
			if h.This == (h.Label == "steward-home") || h.Busy != 1 {
				t.Errorf("home share = %+v", h)
			}
		}
	}
}
