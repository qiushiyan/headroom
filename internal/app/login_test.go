package app

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/headless"
)

// loginWorld is the vendor side of `headroom login`: one stored credential
// and one logged-in email per account, and a login that rewrites them the
// way the approval in a given profile would.
type loginWorld struct {
	now    time.Time
	raw    map[string]string // account name → credential blob
	email  map[string]string // account name → .claude.json's logged-in email
	logins []string          // "name email profileDir", in call order
	// approveAs is who the browser profile is signed in to; "" = the email
	// asked for.
	approveAs map[string]string
	// keychain: accounts whose login is a Keychain item, which raw then
	// holds; the others' raw is the dir's file. A sealed Keychain hands back
	// none of its items, and a read falls back to the file: stray, a login the
	// vendor wrote there while the item kept its own.
	keychain map[string]bool
	stray    map[string]string
	unknown  map[string]bool // accounts whose item `security` cannot answer for
	sealed   bool
	locked   bool // the Keychain hands back no item, though it is not sealed
	remote   bool
	// renewEnd is when an account's new login ends; absent = 30 days out.
	renewEnd map[string]time.Time
}

func blobEnding(token string, end time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		token, end.Add(-29*24*time.Hour).UnixMilli(), end.UnixMilli())
}

func (w *loginWorld) deps() loginDeps {
	return loginDeps{
		health: func([]accounts.Account) auth.QueryFunc {
			return func(string) auth.Status { return auth.Status{} } // no oracle: credential evidence decides
		},
		readCred:  w.readCred,
		metaEmail: func(a accounts.Account) string { return w.email[a.Name] },
		sealed:    w.sealed,
		keychainItem: func(a accounts.Account) (creds.Item, string) {
			switch {
			case w.unknown[a.Name]:
				return creds.ItemUnknown, "security: SecKeychainSearchCopyNext: -25308"
			case w.keychain[a.Name]:
				return creds.ItemPresent, ""
			}
			return creds.ItemAbsent, ""
		},
		approver: approver{profiles: []browser.Profile{{Dir: "Profile 2", Name: "a"}}, remote: w.remote},
		now:      func() time.Time { return w.now },
		tokens:   func() (map[string]headless.Token, error) { return map[string]headless.Token{}, nil },
		login: func(a accounts.Account, _ accounts.Set, page approvalPage) error {
			where := page.profile.Dir
			if page.remote {
				where = "remote"
			}
			w.logins = append(w.logins, a.Name+" "+page.email+" "+where)
			who := page.email
			if as := w.approveAs[a.Name]; as != "" {
				who = as
			}
			end, ok := w.renewEnd[a.Name]
			if !ok {
				end = w.now.Add(30 * 24 * time.Hour)
			}
			w.raw[a.Name] = blobEnding("new-"+a.Name, end)
			w.email[a.Name] = who
			return nil
		},
	}
}

// readCred is what creds.ReadRaw finds: the Keychain item while the Keychain
// answers, else the dir's file.
func (w *loginWorld) readCred(a accounts.Account) (string, creds.Source) {
	if w.keychain[a.Name] {
		if !w.sealed && !w.locked {
			if raw, ok := w.raw[a.Name]; ok {
				return raw, creds.SourceKeychain
			}
		}
		if raw, ok := w.stray[a.Name]; ok {
			return raw, creds.SourceFile
		}
		return "", creds.SourceNone
	}
	if raw, ok := w.raw[a.Name]; ok {
		return raw, creds.SourceFile
	}
	return "", creds.SourceNone
}

func renewFixture(t *testing.T) (config.Scope, *loginWorld) {
	t.Helper()
	cfg := lifecycleConfig(t)
	for _, n := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if got := call(func(std Streams) error { return AccountsAdd(std, cfg, AddOptions{Email: n}) }); got.code != 0 {
			t.Fatalf("seed %s", n)
		}
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	w := &loginWorld{
		now: now,
		raw: map[string]string{
			// the primary has never logged in
			"a@x.com": blobEnding("old-a", now.Add(3*24*time.Hour)),  // ends soon
			"b@x.com": blobEnding("old-b", now.Add(20*24*time.Hour)), // ends later
			"c@x.com": blobEnding("old-c", now.Add(-24*time.Hour)),   // already ended
		},
		email:     map[string]string{"a@x.com": "a@x.com", "b@x.com": "b@x.com", "c@x.com": "c@x.com"},
		approveAs: map[string]string{},
		keychain:  map[string]bool{},
		stray:     map[string]string{},
		unknown:   map[string]bool{},
		renewEnd:  map[string]time.Time{},
	}
	return cfg, w
}

// accountLines are the lines of out that begin with an account's name: its
// plan line, chosen (→) or not, then any headless token line.
func accountLines(out, name string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(strings.TrimPrefix(line, "→")); len(f) > 0 && f[0] == name {
			lines = append(lines, line)
		}
	}
	return lines
}

// planLine is an account's line in the plan, "" when it has none.
func planLine(out, name string) string {
	if lines := accountLines(out, name); len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// resultLine is the line that says how an account's login went.
func resultLine(out, name string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "✓ "+name+":") || strings.HasPrefix(line, "✗ "+name+":") {
			return line
		}
	}
	return ""
}

// A bare login renews on positive evidence only — ending inside the window,
// ended, or never logged in — and leaves the rest alone, in discovery order.
func TestLoginChoosesExpiringAccounts(t *testing.T) {
	cfg, w := renewFixture(t)
	var out, errw bytes.Buffer
	if code := loginTo(&out, &errw, cfg, nil, w.deps()); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errw.String(), out.String())
	}
	want := []string{"primary  ", "a@x.com a@x.com Profile 2", "c@x.com c@x.com "}
	if strings.Join(w.logins, "|") != strings.Join(want, "|") {
		t.Errorf("logins = %q, want %q", w.logins, want)
	}
	if a := planLine(out.String(), "a@x.com"); !strings.Contains(a, "Profile 2") {
		t.Errorf("a's plan line names no profile: %q", a)
	}
	if c := planLine(out.String(), "c@x.com"); !strings.Contains(c, "default browser") {
		t.Errorf("c's plan line does not say the default browser: %q", c)
	}
	if a := resultLine(out.String(), "a@x.com"); !strings.HasPrefix(a, "✓") || !strings.Contains(a, "Nov 5") {
		t.Errorf("a's result: %q\n%s", a, out.String())
	}
}

func TestLoginWindowAllNamesAndDryRun(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--within", "30"}, "primary|a@x.com|b@x.com|c@x.com"},
		{[]string{"--within", "0"}, "primary|c@x.com"},
		{[]string{"--all"}, "primary|a@x.com|b@x.com|c@x.com"},
		{[]string{"b@x.com"}, "b@x.com"},
		{[]string{"--dry-run"}, ""},
	}
	for _, c := range cases {
		cfg, w := renewFixture(t)
		var out, errw bytes.Buffer
		if code := loginTo(&out, &errw, cfg, c.args, w.deps()); code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, code, errw.String())
		}
		var got []string
		for _, l := range w.logins {
			got = append(got, strings.Fields(l)[0])
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%v: logins %q, want %q", c.args, got, c.want)
		}
	}
}

// A batch closes on its earliest end, and a partial failure on one command
// that retries only the logins that ran and failed — not the ones that took,
// and not a held account, which would be refused again.
func TestLoginSummaryNamesTheEarliestEndAndTheRetry(t *testing.T) {
	cfg, w := renewFixture(t)
	w.approveAs["c@x.com"] = "b@x.com"
	w.raw["primary"] = blobEnding("old-p", w.now.Add(24*time.Hour)) // a login, no identity: held
	w.renewEnd["b@x.com"] = w.now.Add(25 * 24 * time.Hour)          // Oct 31, before a's Nov 5
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, cfg, []string{"--all"}, w.deps()); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	closing := ""
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "renewed here") {
			closing = line
		}
	}
	if !strings.Contains(closing, "Oct 31") || strings.Contains(closing, "Nov 5") {
		t.Errorf("the batch closes on %q, want b's Oct 31:\n%s", closing, out.String())
	}
	for _, s := range []string{"2 of 4", "headroom login c@x.com\n"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
}

// What the flags accept — --within's value, --all beside names, an empty
// name — is the command line's: cmd/headroom's TestGrammar.
func TestLoginFlags(t *testing.T) {
	cfg, w := renewFixture(t)
	var errw bytes.Buffer
	if code := loginTo(io.Discard, &errw, cfg, []string{"nobody@x.com"}, w.deps()); code != 1 || !strings.Contains(errw.String(), "not a discovered account") {
		t.Errorf("unknown name: exit %d, %s", code, errw.String())
	}
	if len(w.logins) != 0 {
		t.Errorf("a refused command logged in: %q", w.logins)
	}
}

// Over ssh the login Keychain is sealed: an account whose login lives there
// reads as logged out to the vendor's own probe, and a login made here would
// land in a file the Keychain item outranks. Such an account is never renewed
// from here — not even by name — and the plan says how to open the Keychain.
func TestLoginRefusesAccountsInASealedKeychain(t *testing.T) {
	cfg, w := renewFixture(t)
	w.sealed = true
	w.keychain["a@x.com"] = true
	delete(w.raw, "a@x.com") // what a sealed read returns
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, cfg, nil, w.deps()); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, l := range w.logins {
		if strings.HasPrefix(l, "a@x.com") {
			t.Errorf("renewed a sealed account: %q", w.logins)
		}
	}
	if a := planLine(out.String(), "a@x.com"); !strings.Contains(a, "Keychain") {
		t.Errorf("a's plan line does not say its login is in the Keychain: %q", a)
	}
	if !strings.Contains(out.String(), "security unlock-keychain") {
		t.Errorf("output does not say how to open the Keychain:\n%s", out.String())
	}

	_, w2 := renewFixture(t)
	w2.sealed, w2.keychain["a@x.com"] = true, true
	out.Reset()
	if code := loginTo(&out, io.Discard, cfg, []string{"a@x.com"}, w2.deps()); code != 1 || len(w2.logins) != 0 {
		t.Errorf("named sealed account: exit %d, logins %q, want 1 and none", code, w2.logins)
	}
	// An unsealed Keychain reads normally: the item alone refuses nothing.
	_, w3 := renewFixture(t)
	w3.keychain["a@x.com"] = true
	if code := loginTo(io.Discard, io.Discard, cfg, []string{"a@x.com"}, w3.deps()); code != 0 || len(w3.logins) != 1 {
		t.Errorf("unsealed: exit %d, logins %q", code, w3.logins)
	}
}

// A remote user cannot see this machine's screen: the login is asked for a
// remote page, and the instructions say to open the printed URL and paste the
// code. That nothing opens is pinned through the binary.
// The Keychain is judged on what it gave, not on how macOS words a refusal:
// an item that exists and did not hand back its login holds the account
// with no sealed Keychain in sight (locked), and an item `security` cannot
// answer for holds it too, as unknown.
func TestLoginHoldsWhatTheKeychainDidNotGive(t *testing.T) {
	cfg, w := renewFixture(t)
	w.locked = true
	w.keychain["a@x.com"] = true
	w.unknown["c@x.com"] = true
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, cfg, []string{"a@x.com", "c@x.com"}, w.deps()); code != 1 || len(w.logins) != 0 {
		t.Fatalf("exit %d, logins %q; want 1 and none\n%s", code, w.logins, out.String())
	}
	if line := planLine(out.String(), "a@x.com"); !strings.Contains(line, "login kept in the Keychain, which this session cannot read") {
		t.Errorf("locked: %q", line)
	}
	if line := planLine(out.String(), "c@x.com"); !strings.Contains(line, "could not tell whether its login is in the Keychain (security: ") {
		t.Errorf("unknown: %q", line)
	}
	if !strings.Contains(out.String(), "did not give this session the login of the 1 accounts") || strings.Contains(out.String(), "ssh session") {
		t.Errorf("an unsealed Keychain is not explained as ssh's:\n%s", out.String())
	}
}

// A login renewed into the dir's file while the account's Keychain item
// keeps the old one is not the login sessions use: the Keychain stopped
// answering between the plan and the read-back, the vendor fell back to the
// file, and the file is all a read finds. It did not take. (login-handoff
// spec, test plan 1; read ✓ before)
func TestLoginRenewedIntoTheFileBesideAKeychainItemDidNotTake(t *testing.T) {
	cfg, w := renewFixture(t)
	w.keychain["a@x.com"] = true
	d := w.deps()
	d.login = func(a accounts.Account, _ accounts.Set, _ approvalPage) error {
		w.locked = true
		w.stray[a.Name] = blobEnding("new-"+a.Name, w.now.Add(30*24*time.Hour))
		return nil
	}
	var out bytes.Buffer
	code := loginTo(&out, io.Discard, cfg, []string{"a@x.com"}, d)
	if line := resultLine(out.String(), "a@x.com"); code != 1 || !strings.HasPrefix(line, "✗") || !strings.Contains(line, "Keychain item every session reads first keeps the old one") {
		t.Errorf("exit %d, result %q\n%s", code, line, out.String())
	}
}

// Where the page opens says why no profile was matched: no profile list,
// one that could not be read, or one read with no profile for the email.
func TestThePlanSaysWhyNoProfileMatched(t *testing.T) {
	missing := approver{unread: profileListError{path: "/h/Local State", err: fs.ErrNotExist}}
	unreadable := approver{unread: profileListError{path: "/h/Local State", err: fs.ErrPermission}}
	read := approver{profiles: []browser.Profile{{Dir: "Profile 2", Name: "b"}}}
	for _, c := range []struct {
		ap   approver
		want string
	}{
		{missing, "default browser (no Chrome profile list at /h/Local State)"},
		{unreadable, "default browser (Chrome's profile list at /h/Local State could not be read: permission denied)"},
		{read, "default browser (no Chrome profile matches a@x.com)"},
		{approver{remote: true, unread: missing.unread}, "over ssh: you open the printed URL where you are"},
	} {
		if got := c.ap.page("a@x.com").phrase(); got != c.want {
			t.Errorf("phrase = %q, want %q", got, c.want)
		}
	}
}

func TestLoginRemoteAsksForThePastedCode(t *testing.T) {
	cfg, w := renewFixture(t)
	w.remote = true
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, cfg, []string{"a@x.com"}, w.deps()); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if len(w.logins) != 1 || w.logins[0] != "a@x.com a@x.com remote" {
		t.Errorf("logins = %q, want the remote page", w.logins)
	}
	for _, s := range []string{"over ssh: you open the printed URL where you are", "paste the code it shows"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
}

// vendorStdin is the vendor's end of the relay.
type vendorStdin struct {
	bytes.Buffer
	closed bool
}

func (v *vendorStdin) Close() error { v.closed = true; return nil }

// A paste is read before the vendor sees it: a line that is no code, or a
// code another sign-in issued, is refused with what came in and a fresh
// prompt, and the same line again goes through as pasted, since the check is
// headroom's reading of the vendor. A code from this sign-in passes. The
// vendor's input ends with the terminal's.
func TestRelayCodesChecksEachPaste(t *testing.T) {
	lines := make(chan string)
	vendor, said := &vendorStdin{}, &bytes.Buffer{}
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		relayCodes(lines, vendor, said, func() string { return "THISSTATE0123" }, make(chan struct{}))
	}()
	for _, l := range []string{"", "half a code", "abc#OTHERSTATE9", "abc#OTHERSTATE9", "  good#THISSTATE0123 \r"} {
		lines <- l
	}
	close(lines)
	<-relayed

	if got, want := vendor.String(), "abc#OTHERSTATE9\ngood#THISSTATE0123\n"; got != want {
		t.Errorf("vendor got %q, want %q", got, want)
	}
	if !vendor.closed {
		t.Error("the vendor's input stayed open after the terminal's ended")
	}
	for _, s := range []string{
		`"half a code"`,        // the malformed paste, echoed back
		"OTHERSTA", "THISSTAT", // the two states, told apart
		"as pasted",    // the same line again, sent anyway
		"this sign-in", // the code that passed the check
	} {
		if !strings.Contains(said.String(), s) {
			t.Errorf("relay never said %q:\n%s", s, said.String())
		}
	}
	if n := strings.Count(said.String(), "Paste code here > "); n != 3 {
		t.Errorf("%d fresh prompts, want 3 (the empty line and the two refusals):\n%s", n, said.String())
	}
}

// Ending the login stops the relay and leaves the vendor's input to the
// vendor's exit; with no state recorded, a well-formed code passes unchecked
// and says no more than that.
func TestRelayCodesStopsWithTheLogin(t *testing.T) {
	lines := make(chan string)
	vendor, said := &vendorStdin{}, &bytes.Buffer{}
	done, relayed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(relayed)
		relayCodes(lines, vendor, said, func() string { return "" }, done)
	}()
	lines <- "abc#anything"
	close(done)
	<-relayed
	if vendor.String() != "abc#anything\n" || vendor.closed {
		t.Errorf("vendor got %q (closed %v), want the code and an open pipe", vendor.String(), vendor.closed)
	}
	if !strings.Contains(said.String(), "headroom: code received — passing it to claude") || strings.Contains(said.String(), "this sign-in") {
		t.Errorf("an unchecked code was described as checked:\n%s", said.String())
	}
}

// Lines entered before a login began were meant for an earlier one.
func TestDrainLinesDropsWhatCameBefore(t *testing.T) {
	lines := make(chan string, 2)
	lines <- "old#one"
	lines <- "old#two"
	drainLines(lines)
	if len(lines) != 0 {
		t.Errorf("%d lines left for the new login", len(lines))
	}
	close(lines)
	drainLines(lines) // an ended input is drained, not waited on
}

func TestSSHRemote(t *testing.T) {
	for conn, want := range map[string]bool{
		"":                                     false, // not over ssh
		"100.68.130.84 56302 100.68.130.84 22": false, // the mini into itself
		"127.0.0.1 5000 127.0.0.1 22":          false,
		"::1 5000 ::1 22":                      false,
		"100.68.130.84 56302 100.70.1.2 22":    true, // the mini into the laptop
		"garbage":                              false,
	} {
		if got := sshRemote(conn); got != want {
			t.Errorf("sshRemote(%q) = %v, want %v", conn, got, want)
		}
	}
}

// A login took only when the vendor command succeeded, the stored login is a
// new one, and the dir is now logged in as the account. A live session
// refreshing the access token while the batch waits on other approvals
// changes the credential without any login, so only a new refresh expiry is
// a new login — except in a credential that records none, where the token is
// all there is to go by. (review r1)
func TestLoginReadBack(t *testing.T) {
	noExpiry := func(token string) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":1}}`, token)
	}
	cases := []struct {
		name   string
		before string // a@x.com's credential before; "" = the fixture's
		vendor func(w *loginWorld, a accounts.Account) error
		ok     bool
		want   string // in a@x.com's result line
	}{
		{"a failed vendor command is a failure, whatever changed meanwhile", "", func(w *loginWorld, a accounts.Account) error {
			w.raw[a.Name] = blobEnding("refreshed-"+a.Name, w.now.Add(30*24*time.Hour))
			return fmt.Errorf("exit status 1")
		}, false, "exit status 1"},
		{"an access-token refresh is not a login", "", func(w *loginWorld, a accounts.Account) error {
			w.raw[a.Name] = blobEnding("refreshed-"+a.Name, w.now.Add(3*24*time.Hour))
			return nil
		}, false, "did not change"},
		{"a login approved as someone else did not take", "", func(w *loginWorld, a accounts.Account) error {
			w.raw[a.Name], w.email[a.Name] = blobEnding("new-"+a.Name, w.now.Add(30*24*time.Hour)), "b@x.com"
			return nil
		}, false, "logged in as b@x.com, not a@x.com"},
		{"with no refresh expiry recorded, a new token is a new login", noExpiry("old-a"), func(w *loginWorld, a accounts.Account) error {
			w.raw[a.Name] = noExpiry("new-a")
			return nil
		}, true, "logged in"},
		{"with no refresh expiry recorded, the same token is no login", noExpiry("old-a"), func(*loginWorld, accounts.Account) error {
			return nil
		}, false, "did not change"},
	}
	for _, c := range cases {
		cfg, w := renewFixture(t)
		if c.before != "" {
			w.raw["a@x.com"] = c.before
		}
		d := w.deps()
		d.login = func(a accounts.Account, _ accounts.Set, _ approvalPage) error { return c.vendor(w, a) }
		var out bytes.Buffer
		code := loginTo(&out, io.Discard, cfg, []string{"a@x.com"}, d)
		line := resultLine(out.String(), "a@x.com")
		if (code == 0) != c.ok || strings.HasPrefix(line, "✓") != c.ok || !strings.Contains(line, c.want) {
			t.Errorf("%s: exit %d, result %q, want ok=%v with %q\n%s", c.name, code, line, c.ok, c.want, out.String())
		}
	}
}

// A primary with a login but no readable identity cannot have its new login
// checked, so it is held back — unless the vendor's own status names it.
// (review r1)
func TestLoginHoldsAPrimaryWhoseIdentityIsUnknown(t *testing.T) {
	cfg, w := renewFixture(t)
	w.raw["primary"] = blobEnding("old-p", w.now.Add(24*time.Hour))
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, cfg, []string{"primary"}, w.deps()); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	if len(w.logins) != 0 || !strings.Contains(out.String(), "identity unreadable") {
		t.Errorf("logins %q\n%s", w.logins, out.String())
	}

	d := w.deps()
	d.health = func([]accounts.Account) auth.QueryFunc {
		return func(dir string) auth.Status {
			if dir == "" {
				return auth.Status{Outcome: auth.OutcomeOK, LoggedIn: true, Email: "p@x.com"}
			}
			return auth.Status{}
		}
	}
	w.approveAs["primary"] = "a@x.com"
	out.Reset()
	if code := loginTo(&out, io.Discard, cfg, []string{"primary"}, d); code != 1 || !strings.Contains(out.String(), "logged in as a@x.com, not p@x.com") {
		t.Errorf("primary not checked against the vendor's email: exit %d\n%s", code, out.String())
	}
}

// The opener's variables are headroom's to set, never inherited: each appears
// once, with this sign-in's value. A shell vendor keeps the last of two
// duplicate entries, which is why the binary tests cannot see this, but a Go
// or Node child reads the first — the inherited one — and would record the
// sign-in in some other run's file. Off macOS at the machine nothing is set,
// and an inherited value is still dropped.
func TestTheOpenerEnvironmentIsNeverInherited(t *testing.T) {
	base := []string{
		"PATH=/bin", browser.SigninEnv + "=/inherited/signin", browser.ProfileEnv + "=Profile 9", "BROWSER=firefox",
	}
	count := func(env []string, name string) (n int, value string) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				n, value = n+1, v
			}
		}
		return n, value
	}
	for _, pg := range []approvalPage{{remote: true}, {profile: browser.Profile{Dir: "Profile 7"}}} {
		env, err := pg.env(base, "/mine/signin")
		if err != nil {
			t.Fatal(err)
		}
		if !pg.recorded() {
			if n, _ := count(env, browser.SigninEnv); n != 0 {
				t.Errorf("remote=%v: an inherited sign-in reached a vendor headroom does not open for: %q", pg.remote, env)
			}
			continue
		}
		if n, v := count(env, browser.SigninEnv); n != 1 || v != "/mine/signin" {
			t.Errorf("remote=%v: %s appears %d times, last %q: %q", pg.remote, browser.SigninEnv, n, v, env)
		}
		if n, _ := count(env, "BROWSER"); n != 1 {
			t.Errorf("remote=%v: BROWSER appears %d times: %q", pg.remote, n, env)
		}
		want := 1
		if pg.remote {
			want = 0
		}
		if n, _ := count(env, browser.ProfileEnv); n != want {
			t.Errorf("remote=%v: %s appears %d times, want %d: %q", pg.remote, browser.ProfileEnv, n, want, env)
		}
	}
}

// loginTo renews through the login surface, the way the command line spells
// its flags.
func loginTo(out, errw io.Writer, cfg config.Scope, args []string, deps loginDeps) int {
	opts := LoginOptions{Within: DefaultLoginWithin}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--all":
			opts.All = true
		case "--dry-run":
			opts.DryRun = true
		case "--within":
			i++
			days, _ := strconv.Atoi(args[i])
			opts.Within = time.Duration(days) * 24 * time.Hour
		default:
			opts.Names = append(opts.Names, a)
		}
	}
	r := call(func(std Streams) error { return renewLogins(std, cfg, opts, deps) })
	io.WriteString(out, r.stdout)
	io.WriteString(errw, r.stderr)
	return r.code
}
