package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/placement"
)

const testToken = "sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKlMnOpQrSt"

// mint stores a headless token for an account of the fixture, minted for the
// subscription its dir is logged in to and ending `left` from now.
func (f autoFixture) mint(name string, left time.Duration) {
	f.t.Helper()
	tok := headless.Token{Value: testToken + tokenSuffix(name), MintedMS: time.Now().UnixMilli(), EndsMS: time.Now().Add(left).UnixMilli(), Key: f.key(name).ID()}
	if err := headless.Put(f.cfg.AccountsRoot, name, tok); err != nil {
		f.t.Fatal(err)
	}
}

// tokenSuffix makes each account's test token its own, in the token alphabet.
func tokenSuffix(name string) string {
	return strings.NewReplacer("@", "_", ".", "_").Replace(name)
}

func (f autoFixture) place(args ...string) (headlessDoc, int, string) {
	f.t.Helper()
	opts := HeadlessPlaceOptions{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			opts.DryRun = true
		case "--prefer":
			i++
			opts.Prefer = args[i]
		case "--exclude":
			i++
			opts.Exclude = append(opts.Exclude, args[i])
		default:
			f.t.Fatalf("place flag %q is not one these tests spell", args[i])
		}
	}
	r := call(func(std Streams) error { return HeadlessPlace(std, f.cfg, opts, f.edges) })
	var doc headlessDoc
	if r.stdout != "" {
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			f.t.Fatalf("place printed no document: %v\n%s", err, r.stdout)
		}
	}
	return doc, r.code, r.stderr
}

func (d headlessDoc) find(name string) headlessAcct {
	for _, a := range d.Accounts {
		if a.Account == name {
			return a
		}
	}
	return headlessAcct{}
}

// A headless placement is the automatic launch's rule over the accounts that
// hold a token: recorded as load under an id, logged as a headless line, with
// a refresh left behind — and the user's own launches then count it.
func TestAHeadlessPlacementIsTheRuleOverTokenHolders(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com")
	f.observe("a@x.com", 20, 10, time.Minute)
	f.observe("b@x.com", 10, 10, time.Minute)
	f.observe("c@x.com", 0, 0, time.Minute) // emptiest, and no token
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)

	doc, code, errw := f.place()
	if code != 0 || !doc.Placed || doc.Account != "b@x.com" || doc.Reason != placement.ReasonLeastLoad || doc.Placement == "" {
		t.Fatalf("code %d doc %+v stderr %s", code, doc, errw)
	}
	if doc.TokenEndsAt == nil || doc.RetryAt != nil || doc.Rule != placement.Rule {
		t.Errorf("ends %v retry %v rule %q", doc.TokenEndsAt, doc.RetryAt, doc.Rule)
	}
	for _, name := range []string{"c@x.com", "qiushi"} {
		if a := doc.find(name); a.Eligible || !strings.HasPrefix(a.Excluded, "no headless token") || !strings.Contains(a.Excluded, "headroom headless mint "+name) {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if b := doc.find("b@x.com"); b.Session == nil || b.Session.Percent != 10 || b.Session.Basis != "observed" || b.ObservedAt == nil {
		t.Errorf("b's figures: %+v", b)
	}
	if out, _ := json.Marshal(doc); strings.Contains(string(out), "sk-ant-") {
		t.Fatal("the placement document carries a token")
	}

	rec := f.st.Load().Placements().Recent
	if len(rec) != 1 || rec[0].ID != doc.Placement || rec[0].PID != 0 || rec[0].Name != "b@x.com" {
		t.Fatalf("recorded %+v", rec)
	}
	log := f.log()
	if len(log) != 1 || log[0].Mode != "headless" || !log[0].Automatic || log[0].Chosen != "b@x.com" || !log[0].Recorded {
		t.Fatalf("log %+v", log)
	}
	if *f.refreshes != 1 {
		t.Errorf("%d refreshes left behind", *f.refreshes)
	}

	// The next one spreads — b's placement is load on b — and the user's own
	// launch counts both, and goes where neither went.
	if doc, _, _ := f.place(); doc.Account != "a@x.com" {
		t.Errorf("second placement went to %q", doc.Account)
	}
	marks := placementView(accounts.Discover(f.cfg), f.st, time.Now(), f.edges)
	if n := marks["a@x.com"].Load.Launched; n != 1 {
		t.Errorf("a launch counts a's headless placement as %d", n)
	}
	if n := marks["b@x.com"].Load.Launched; n != 1 {
		t.Errorf("a launch counts b's headless placement as %d", n)
	}
	if !marks["c@x.com"].Next {
		t.Errorf("a launch would not go to the account eval work left alone: %+v", marks)
	}
}

// When every account a placement could take is near a limit it refuses — no
// record, no log line — and says when the first has room again.
func TestAHeadlessPlacementWaitsRatherThanTakeAnAccountNearALimit(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("a@x.com", 85, 10, time.Minute) // its session resets in two hours
	f.observeWeek("b@x.com", 5, 92, 30*time.Hour, time.Minute)
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)

	doc, code, _ := f.place()
	if code != ExitRefused || doc.Placed || doc.Placement != "" || !strings.Contains(doc.Refusal, "near a limit") {
		t.Fatalf("code %d doc %+v", code, doc)
	}
	retry, err := time.Parse(time.RFC3339, deref(doc.RetryAt))
	if err != nil || retry.Sub(time.Now()) < 110*time.Minute || retry.Sub(time.Now()) > 2*time.Hour {
		t.Errorf("retry at %v (%v), want a's session reset about two hours out", doc.RetryAt, err)
	}
	if a := doc.find("a@x.com"); !a.NearLimit || !a.Eligible || a.RoomAt == "" {
		t.Errorf("a: %+v", a)
	}
	if n := len(f.st.Load().Placements().Recent); n != 0 || len(f.log()) != 0 {
		t.Fatalf("a refusal left %d placements and %d log lines", n, len(f.log()))
	}

	// Nothing near a limit to wait for: refused, and no instant.
	g := newAutoFixture(t, "a@x.com")
	doc, code, _ = g.place()
	if code != ExitRefused || doc.RetryAt != nil || !strings.Contains(doc.Refusal, "no headless token") {
		t.Fatalf("no tokens: %d %+v", code, doc)
	}
}

// A placement that cannot be recorded is not handed out: a session nobody
// can see is one the owner's launches cannot steer around. Handing out the
// decision when state.Place fails must fail this test.
func TestAnUnrecordedPlacementIsNotHandedOut(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.mint("a@x.com", 300*24*time.Hour)
	if err := os.Chmod(f.cfg.AccountsRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.cfg.AccountsRoot, 0o755) })
	doc, code, errw := f.place()
	if code != 1 || doc.Placed || doc.Account != "" || doc.Placement != "" || doc.Refusal == "" || errw == "" {
		t.Fatalf("code %d doc %+v stderr %q", code, doc, errw)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A session reported against its placement counts as busy for as long as its
// process runs — past the fifteen minutes a placement counts — and once; when
// it exits, the next write drops its record.
func TestAStartedSessionCountsAsBusyForItsWholeLife(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)
	doc, _, _ := f.place()
	f.live[4242] = time.Now().Unix()
	var errw bytes.Buffer
	if code := f.started(&errw, doc.Placement, 4242); code != 0 {
		t.Fatalf("started: %d %s", code, errw.String())
	}
	runs := f.st.Load().Runs()
	if len(runs) != 1 || runs[0].PID != 4242 || runs[0].Name != doc.Account || runs[0].Home != f.st.Home() {
		t.Fatalf("runs %+v", runs)
	}

	count := func(at time.Time) *accountstate.Load {
		return placementView(accounts.Discover(f.cfg), f.st, at, f.edges)[doc.Account].Load
	}
	if c := count(time.Now()); c.Busy != 1 || c.Launched != 0 || c.Value != 1 {
		t.Fatalf("running: busy %d pending %d load %d", c.Busy, c.Launched, c.Value)
	}
	if c := count(time.Now().Add(40 * time.Minute)); c.Busy != 1 || c.Value != 1 {
		t.Fatalf("forty minutes in: busy %d load %d", c.Busy, c.Value)
	}

	// The process exits: it is no longer busy, its placement counts out its
	// span, and the next placement drops the run.
	delete(f.live, 4242)
	if c := count(time.Now()); c.Busy != 0 || c.Launched != 1 {
		t.Fatalf("exited: busy %d pending %d", c.Busy, c.Launched)
	}
	f.place()
	if runs := f.st.Load().Runs(); len(runs) != 0 {
		t.Fatalf("a run whose process exited was kept: %+v", runs)
	}

	// A process gone before it is reported has nothing more to count; an id
	// nobody placed is an error.
	errw.Reset()
	if code := f.started(&errw, doc.Placement, 999); code != 0 || !strings.Contains(errw.String(), "already exited") {
		t.Errorf("gone process: %d %s", code, errw.String())
	}
	f.live[777] = time.Now().Unix()
	errw.Reset()
	if code := f.started(&errw, "nope", 777); code != 1 || !strings.Contains(errw.String(), "no recent placement") {
		t.Errorf("unknown placement: %d %s", code, errw.String())
	}
}

// A token is excluded on positive evidence: it has ended, or the dir is now
// logged in to another subscription or to none, so its figures cannot be read.
func TestHeadlessExclusions(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com")
	f.mint("a@x.com", -time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)
	f.mint("c@x.com", 300*24*time.Hour)
	writeJSON(t, f.cfg.AccountsRoot+"/b@x.com/.claude.json", `{"oauthAccount":{"emailAddress":"b@x.com","accountUuid":"u-other"}}`)
	writeJSON(t, f.cfg.AccountsRoot+"/c@x.com/.claude.json", `{}`)

	doc, code, _ := f.place("--dry-run")
	if code != ExitRefused || !doc.DryRun {
		t.Fatalf("code %d %+v", code, doc)
	}
	for name, want := range map[string]string{
		"a@x.com": "headless token ended",
		"b@x.com": "logged in to another subscription",
		"c@x.com": "not logged in",
	} {
		if a := doc.find(name); !strings.Contains(a.Excluded, want) {
			t.Errorf("%s excluded %q, want %q", name, a.Excluded, want)
		}
	}
}

// --prefer follows the account a session ran on while it has room, as a
// launch follows the owner of a session it names.
func TestAPreferredAccountIsFollowedWhileItHasRoom(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("a@x.com", 40, 10, time.Minute)
	f.observe("b@x.com", 0, 0, time.Minute)
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)
	if doc, _, _ := f.place("--prefer", "a@x.com", "--dry-run"); doc.Account != "a@x.com" || doc.Reason != placement.ReasonOwner {
		t.Errorf("preferred: %q %s", doc.Account, doc.Reason)
	}
	g := newAutoFixture(t, "a@x.com", "b@x.com")
	g.observe("a@x.com", 90, 10, time.Minute)
	g.mint("a@x.com", 300*24*time.Hour)
	g.mint("b@x.com", 300*24*time.Hour)
	if doc, _, _ := g.place("--prefer", "a@x.com", "--dry-run"); doc.Account != "b@x.com" || doc.Reason != placement.ReasonMoved {
		t.Errorf("preferred near a limit: %q %s", doc.Account, doc.Reason)
	}
	if _, code, errw := f.place("--prefer", "nobody@x.com"); code != 1 || !strings.Contains(errw, "nobody@x.com") {
		t.Errorf("unknown preference: %d %s", code, errw)
	}
}

// The token leaves its file only on a request naming its account, and only
// when it still works for that account.
func TestTheTokenIsPrintedOnlyWhenAsked(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com", "d@x.com", "e@x.com")
	for _, n := range []string{"a@x.com", "c@x.com", "d@x.com", "e@x.com"} {
		f.mint(n, 300*24*time.Hour)
	}
	f.mint("b@x.com", -time.Hour)
	writeJSON(t, f.cfg.AccountsRoot+"/c@x.com/.claude.json", `{"oauthAccount":{"emailAddress":"c@x.com","accountUuid":"u-other"}}`)
	// Positive evidence only: a dir logged out, or whose identity cannot be
	// read just now, still holds the subscription its token was minted for.
	writeJSON(t, f.cfg.AccountsRoot+"/d@x.com/.claude.json", `{}`)
	writeJSON(t, f.cfg.AccountsRoot+"/e@x.com/.claude.json", `{torn`)

	var out, errw bytes.Buffer
	for _, n := range []string{"a@x.com", "d@x.com", "e@x.com"} {
		out.Reset()
		if code := f.token(&out, &errw, n); code != 0 || out.String() != testToken+tokenSuffix(n)+"\n" {
			t.Fatalf("token %s: %d %q %s", n, code, out.String(), errw.String())
		}
	}
	for name, want := range map[string]string{
		"b@x.com": "ended", "c@x.com": "another subscription", "qiushi": "no headless token", "nobody@x.com": "nobody@x.com",
	} {
		out.Reset()
		errw.Reset()
		if code := f.token(&out, &errw, name); code != 1 || out.Len() != 0 || !strings.Contains(errw.String(), want) {
			t.Errorf("%s: %d %q %q", name, code, out.String(), errw.String())
		}
	}

	// The listing names every account and says when each token ends; it
	// never shows one.
	out.Reset()
	if code := f.list(&out, false); code != 0 || strings.Contains(out.String(), "sk-ant-") ||
		!strings.Contains(out.String(), "none — headroom headless mint qiushi") || !strings.Contains(out.String(), "headless token ended") {
		t.Fatalf("listing %d:\n%s", code, out.String())
	}
	out.Reset()
	f.list(&out, true)
	if strings.Contains(out.String(), "sk-ant-") || !strings.Contains(out.String(), `"ends_at"`) {
		t.Fatalf("json listing:\n%s", out.String())
	}

	out.Reset()
	if code := call(func(std Streams) error { return HeadlessForget(std, f.cfg, "a@x.com") }).code; code != 0 {
		t.Fatalf("forget: %d", code)
	}
	if code := f.token(io.Discard, io.Discard, "a@x.com"); code != 1 {
		t.Error("a forgotten token was printed")
	}
}

// mintWorld is the vendor's setup-token as its output reaches a pipe: whole
// frames of plain lines, the token on the success frame.
type mintWorld struct {
	frames []string
	err    error
	pages  []approvalPage
}

func (w *mintWorld) deps(now time.Time) mintDeps {
	return mintDeps{
		terminal: true,
		approver: approver{profiles: []browser.Profile{{Dir: "Profile 2", Name: "a"}}},
		now:      func() time.Time { return now },
		wait:     defaultMintTimes.wait,
		setupToken: func(_ config.Scope, page approvalPage, out *headless.Scanner) error {
			w.pages = append(w.pages, page)
			for _, f := range w.frames {
				io.WriteString(out, f)
			}
			return w.err
		},
	}
}

const (
	frameWelcome = "Welcome to Claude Code v2.1.296\n\n This will guide you through long-lived (1-year) auth token setup for your Claude account.\n"
	frameSuccess = " ✓ Long-lived authentication token created successfully!\n\n Your OAuth token (valid for 1 year):\n\n " + testToken + "\n\n Store this token securely.\n"
)

// A mint stores what the vendor printed — never showing it — with the end the
// vendor stated, against the subscription the dir is logged in to.
func TestMintStoresTheTokenTheVendorPrinted(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	now := time.Now()
	w := &mintWorld{frames: []string{frameWelcome, frameWelcome + frameSuccess}}
	var out, errw bytes.Buffer
	if code := mintTo(&out, &errw, f.cfg, "a@x.com", w.deps(now)); code != 0 {
		t.Fatalf("mint: %d\n%s%s", code, out.String(), errw.String())
	}
	if strings.Contains(out.String(), "sk-ant-") || !strings.Contains(out.String(), headless.Withheld) || !strings.Contains(out.String(), "✓ a@x.com: headless token stored") {
		t.Fatalf("output:\n%s", out.String())
	}
	if len(w.pages) != 1 || w.pages[0].profile.Dir != "Profile 2" || w.pages[0].remote {
		t.Errorf("page %+v", w.pages)
	}
	tokens, err := headless.Read(f.cfg.AccountsRoot)
	tok := tokens["a@x.com"]
	if err != nil || tok.Value != testToken || tok.Key != f.key("a@x.com").ID() || tok.Email != "a@x.com" {
		t.Fatalf("stored %+v %v", tok, err)
	}
	if got := tok.EndsMS - now.UnixMilli(); got != (365 * 24 * time.Hour).Milliseconds() {
		t.Errorf("ends in %v", time.Duration(got)*time.Millisecond)
	}

	// The primary's dir is not named by its email: its login is whoever its
	// identity says, and the token is counted against that.
	wp := &mintWorld{frames: []string{frameSuccess}}
	if code := mintTo(io.Discard, &errw, f.cfg, "qiushi", wp.deps(now)); code != 0 {
		t.Fatalf("mint for the primary: %d %s", code, errw.String())
	}
	if tokens, err := headless.Read(f.cfg.AccountsRoot); err != nil || tokens["qiushi"].Key != f.key("qiushi").ID() || tokens["qiushi"].Email != "qiushi@x.com" {
		t.Fatalf("the primary's token: %+v %v", tokens["qiushi"], err)
	}

	// A run that printed no token stores nothing and replaces nothing.
	w2 := &mintWorld{frames: []string{frameWelcome, " OAuth error: Request failed with status code 400\n"}, err: &exitError{1}}
	out.Reset()
	if code := mintTo(&out, &errw, f.cfg, "a@x.com", w2.deps(now.Add(time.Hour))); code != 1 || !strings.Contains(out.String(), "no headless token was minted") {
		t.Fatalf("failed mint: %d\n%s", code, out.String())
	}
	if again, _ := headless.Read(f.cfg.AccountsRoot); again["a@x.com"] != tok {
		t.Error("a failed mint changed the stored token")
	}
}

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit status 1" }

// A mint for a remote user opens nothing here; one for an account whose dir
// holds no login of its own, or off a terminal, is refused before the vendor
// runs.
func TestMintRefusalsAndARemoteUser(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com")
	writeJSON(t, f.cfg.AccountsRoot+"/b@x.com/.claude.json", `{}`)
	writeJSON(t, f.cfg.AccountsRoot+"/c@x.com/.claude.json", `{"oauthAccount":{"emailAddress":"z@x.com","accountUuid":"u-z"}}`)
	now := time.Now()
	for name, want := range map[string]string{"b@x.com": "not logged in", "c@x.com": "logged in as z@x.com"} {
		w := &mintWorld{frames: []string{frameSuccess}}
		var errw bytes.Buffer
		if code := mintTo(io.Discard, &errw, f.cfg, name, w.deps(now)); code != 1 || len(w.pages) != 0 || !strings.Contains(errw.String(), want) {
			t.Errorf("%s: %d %s (ran %d)", name, code, errw.String(), len(w.pages))
		}
	}
	// A token file that could not take the token refuses before the vendor
	// runs: a token minted and not stored is lost.
	if err := os.WriteFile(headless.Path(f.cfg.AccountsRoot), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &mintWorld{frames: []string{frameSuccess}}
	var torn bytes.Buffer
	if code := mintTo(io.Discard, &torn, f.cfg, "a@x.com", w.deps(now)); code != 1 || len(w.pages) != 0 {
		t.Errorf("torn token file: %d %s (ran %d)", code, torn.String(), len(w.pages))
	}
	os.Remove(headless.Path(f.cfg.AccountsRoot))

	deps := w.deps(now)
	deps.terminal = false
	var errw bytes.Buffer
	if code := mintTo(io.Discard, &errw, f.cfg, "a@x.com", deps); code != 1 || len(w.pages) != 0 {
		t.Errorf("off a terminal: %d %s", code, errw.String())
	}
	deps = w.deps(now)
	deps.approver.remote = true
	var out bytes.Buffer
	if code := mintTo(&out, io.Discard, f.cfg, "a@x.com", deps); code != 0 || !w.pages[0].remote || w.pages[0].profile.Dir != "" || !strings.Contains(out.String(), "paste the code it shows") {
		t.Errorf("remote: %d %+v\n%s", code, w.pages, out.String())
	}
}

// The login plan names the headless tokens that end inside its window, the
// way it names logins.
func TestTheLoginPlanNamesTokensEndingSoon(t *testing.T) {
	cfg, w := renewFixture(t)
	deps := w.deps()
	deps.tokens = func() (map[string]headless.Token, error) {
		return map[string]headless.Token{
			"a@x.com": {Value: testToken, EndsMS: w.now.Add(3 * 24 * time.Hour).UnixMilli()},
			"b@x.com": {Value: testToken, EndsMS: w.now.Add(200 * 24 * time.Hour).UnixMilli()},
			"c@x.com": {Value: testToken, EndsMS: w.now.Add(-24 * time.Hour).UnixMilli()},
		}, nil
	}
	var out bytes.Buffer
	loginTo(&out, io.Discard, cfg, []string{"--dry-run"}, deps)
	tokenLine := func(name string) string {
		for _, l := range accountLines(out.String(), name) {
			if strings.Contains(l, "headless token") {
				return l
			}
		}
		return ""
	}
	for name, want := range map[string]string{"a@x.com": "ends", "c@x.com": "ended"} {
		if l := tokenLine(name); !strings.Contains(l, want) || !strings.Contains(l, "headroom headless mint "+name) {
			t.Errorf("%s's token line %q, want it %s with the mint command:\n%s", name, l, want, out.String())
		}
	}
	if l := tokenLine("b@x.com"); l != "" {
		t.Errorf("a token far from its end was named: %q", l)
	}
}

// The vendor ignores every key while it waits for the approval, so a mint
// ends it: after the wait with no approval, or shortly after it has printed
// the token if it is still running then. Either way the scratch dir goes.
func TestMintEndsAVendorThatDoesNotExit(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())
	times := mintTimes{wait: 400 * time.Millisecond, grace: 200 * time.Millisecond}
	scope := claudeScope(t.TempDir(), "qiushi")

	write := func(body string) {
		if err := os.WriteFile(bin+"/claude", []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("echo waiting; exec sleep 30\n")
	scan := headless.NewScanner(io.Discard)
	start := time.Now()
	err := times.setupToken(os.Stdin, scope, approvalPage{remote: true}, scan)
	if err == nil || !strings.Contains(err.Error(), "no approval within") || time.Since(start) > 5*time.Second {
		t.Fatalf("unapproved: %v after %v", err, time.Since(start))
	}

	// The approval wait is long here: a freshly written script can take
	// seconds to print its first byte on macOS, and this case is about the
	// grace after the token, not the wait before it. The bound is the
	// vendor's own 30-second sleep, which the mint must not sit out.
	times.wait = time.Minute
	write("printf ' Your OAuth token (valid for 1 year):\\n\\n " + testToken + "\\n'; exec sleep 30\n")
	scan = headless.NewScanner(io.Discard)
	start = time.Now()
	if err := times.setupToken(os.Stdin, scope, approvalPage{remote: true}, scan); err != nil || scan.Token() != testToken || time.Since(start) > 20*time.Second {
		t.Fatalf("lingering after the token: %v, token %q, after %v", err, scan.Token(), time.Since(start))
	}
	if left, _ := os.ReadDir(os.Getenv("TMPDIR")); len(left) != 0 {
		t.Errorf("scratch dirs left: %v", left)
	}
}

// An account the caller will not take is excluded inside the placement: the
// rule decides among the rest, the account reads ineligible with its reason,
// its resets say nothing about when room returns, and with every account
// excluded the refusal records nothing and gives no instant to wait for.
func TestACallerExcludesAccountsItWillNotTake(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com", "c@x.com")
	f.observe("a@x.com", 0, 0, time.Minute)                     // the rule's choice, until excluded
	f.observe("b@x.com", 20, 10, time.Minute)                   // what is left
	f.observeWeek("c@x.com", 90, 10, 72*time.Hour, time.Minute) // near a limit, back in two hours
	for _, n := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		f.mint(n, 300*24*time.Hour)
	}
	doc, code, _ := f.place("--exclude", "a@x.com")
	if code != 0 || doc.Account != "b@x.com" {
		t.Fatalf("placed %q (%d), want the best of the rest", doc.Account, code)
	}
	if a := doc.find("a@x.com"); a.Eligible || a.Excluded != "excluded by the caller" {
		t.Errorf("a: %+v", a)
	}
	if rec := f.st.Load().Placements().Recent; len(rec) != 1 || rec[0].Name != "b@x.com" {
		t.Fatalf("recorded %+v", rec)
	}
	if l := f.log(); len(l) != 1 || loggedCandidate(l[0], "a@x.com").Excluded != "excluded by the caller" {
		t.Errorf("the log does not carry the exclusion: %+v", l)
	}

	// Excluding the near account leaves no instant: what is left is open or
	// excluded, and an excluded account's reset is no reason to wait.
	doc, code, _ = f.place("--exclude", "a@x.com", "--exclude", "b@x.com", "--dry-run")
	if code != ExitRefused || doc.RetryAt == nil {
		t.Fatalf("c is near a limit and left: %d %+v", code, doc)
	}
	doc, code, _ = f.place("--exclude", "a@x.com", "--exclude", "b@x.com", "--exclude", "c@x.com")
	if code != ExitRefused || doc.RetryAt != nil || doc.Placement != "" {
		t.Fatalf("all excluded: %d %+v", code, doc)
	}
	for _, want := range []string{"a@x.com (excluded by the caller)", "c@x.com (excluded by the caller)", "qiushi (no headless token"} {
		if !strings.Contains(doc.Refusal, want) {
			t.Errorf("refusal %q lacks %q", doc.Refusal, want)
		}
	}
	if n := len(f.st.Load().Placements().Recent); n != 1 {
		t.Errorf("a refusal recorded a placement: %d", n)
	}

	if _, code, errw := f.place("--exclude", "nobody@x.com"); code != 1 || !strings.Contains(errw, "nobody@x.com") {
		t.Errorf("unknown exclusion: %d %s", code, errw)
	}
	// --exclude with no name is the grammar's refusal: cmd/headroom's TestGrammar.
	if _, code, errw := f.place("--prefer", "b@x.com", "--exclude", "b@x.com"); code != 2 || !strings.Contains(errw, "both preferred and excluded") {
		t.Errorf("preferred and excluded: %d %s", code, errw)
	}
}

// started reports a process on a placement through the started surface.
func (f autoFixture) started(errw *bytes.Buffer, id string, pid int) int {
	r := call(func(std Streams) error { return HeadlessStarted(std, f.cfg, id, pid, f.edges) })
	errw.WriteString(r.stderr)
	return r.code
}

// token prints an account's token through the token surface.
func (f autoFixture) token(out, errw io.Writer, name string) int {
	r := call(func(std Streams) error { return HeadlessToken(std, f.cfg, name) })
	io.WriteString(out, r.stdout)
	io.WriteString(errw, r.stderr)
	return r.code
}

// list prints the token listing.
func (f autoFixture) list(out *bytes.Buffer, asJSON bool) int {
	r := call(func(std Streams) error { return HeadlessList(std, f.cfg, asJSON, time.Now()) })
	out.WriteString(r.stdout)
	return r.code
}

// mintTo mints through the mint surface with the test's edges.
func mintTo(out, errw io.Writer, cfg config.Scope, name string, deps mintDeps) int {
	r := call(func(std Streams) error { return headlessMint(std, cfg, name, deps) })
	io.WriteString(out, r.stdout)
	io.WriteString(errw, r.stderr)
	return r.code
}
