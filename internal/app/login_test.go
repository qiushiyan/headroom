package app

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
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
	keychain  map[string]bool // accounts with a Keychain item
	sealed    bool
	remote    bool
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
		readRaw:    func(a accounts.Account) string { return w.raw[a.Name] },
		metaEmail:  func(a accounts.Account) string { return w.email[a.Name] },
		sealed:     w.sealed,
		inKeychain: func(a accounts.Account) bool { return w.keychain[a.Name] },
		profiles:   []browser.Profile{{Dir: "Profile 2", Name: "a"}},
		remote:     w.remote,
		now:        func() time.Time { return w.now },
		login: func(a accounts.Account, _ accounts.Set, email string, page approvalPage) error {
			where := page.profileDir
			if page.remote {
				where = "remote"
			}
			w.logins = append(w.logins, a.Name+" "+email+" "+where)
			who := email
			if as := w.approveAs[a.Name]; as != "" {
				who = as
			}
			w.raw[a.Name] = blobEnding("new-"+a.Name, w.now.Add(30*24*time.Hour))
			w.email[a.Name] = who
			return nil
		},
	}
}

func renewFixture(t *testing.T) (config.Scope, *loginWorld) {
	t.Helper()
	cfg := lifecycleConfig(t)
	for _, n := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if code := runAccountsAddTo(io.Discard, io.Discard, cfg, []string{n}); code != 0 {
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
	}
	return cfg, w
}

// A bare login renews on positive evidence only — ending inside the window,
// ended, or never logged in — and leaves the rest alone, in discovery order.
func TestLoginChoosesExpiringAccounts(t *testing.T) {
	cfg, w := renewFixture(t)
	var out, errw bytes.Buffer
	if code := runLoginTo(&out, &errw, cfg, nil, w.deps()); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errw.String(), out.String())
	}
	want := []string{"primary  ", "a@x.com a@x.com Profile 2", "c@x.com c@x.com "}
	if strings.Join(w.logins, "|") != strings.Join(want, "|") {
		t.Errorf("logins = %q, want %q", w.logins, want)
	}
	for _, s := range []string{"Chrome profile a (Profile 2)", "default browser (no Chrome profile matches c@x.com)", "✓ a@x.com: logged in, login ends Nov 5",
		"the logins renewed here end from Nov 5 (in 30d)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "b@x.com: logged in") {
		t.Errorf("b@x.com renewed outside the window:\n%s", out.String())
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
		if code := runLoginTo(&out, &errw, cfg, c.args, w.deps()); code != 0 {
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

// The wrong-profile case: the dir took a login, but as someone else. The
// command fails the account and says how to fix it.
func TestLoginReadBackCatchesWrongAccount(t *testing.T) {
	cfg, w := renewFixture(t)
	w.approveAs["a@x.com"] = "b@x.com"
	var out, errw bytes.Buffer
	if code := runLoginTo(&out, &errw, cfg, []string{"a@x.com"}, w.deps()); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ a@x.com: logged in as b@x.com, not a@x.com") {
		t.Errorf("wrong-account not reported:\n%s", out.String())
	}
}

// A batch closes on its earliest end, and a partial failure on one command
// that retries only the logins that ran and failed — not the ones that took,
// and not a held account, which would be refused again.
func TestLoginSummaryNamesTheEarliestEndAndTheRetry(t *testing.T) {
	cfg, w := renewFixture(t)
	w.approveAs["c@x.com"] = "b@x.com"
	w.raw["primary"] = blobEnding("old-p", w.now.Add(24*time.Hour)) // a login, no identity: held
	var out bytes.Buffer
	if code := runLoginTo(&out, io.Discard, cfg, []string{"--all"}, w.deps()); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	for _, s := range []string{"the logins renewed here end from Nov 5", "2 of 4 logins did not take", "retry them: headroom login c@x.com\n"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
}

// A login that ran but stored nothing new did not take, whatever the exit.
func TestLoginReadBackCatchesUnchangedCredential(t *testing.T) {
	cfg, w := renewFixture(t)
	d := w.deps()
	d.login = func(accounts.Account, accounts.Set, string, approvalPage) error { return nil }
	var out bytes.Buffer
	if code := runLoginTo(&out, io.Discard, cfg, []string{"a@x.com"}, d); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out.String(), "✗ a@x.com: the stored login did not change") {
		t.Errorf("unchanged credential not reported:\n%s", out.String())
	}
}

func TestLoginFlags(t *testing.T) {
	cfg, w := renewFixture(t)
	for _, args := range [][]string{{"--within"}, {"--within", "x"}, {"--bogus"}, {"--all", "a@x.com"}} {
		var errw bytes.Buffer
		if code := runLoginTo(io.Discard, &errw, cfg, args, w.deps()); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	var errw bytes.Buffer
	if code := runLoginTo(io.Discard, &errw, cfg, []string{"nobody@x.com"}, w.deps()); code != 1 || !strings.Contains(errw.String(), "not a discovered account") {
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
	if code := runLoginTo(&out, io.Discard, cfg, nil, w.deps()); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, l := range w.logins {
		if strings.HasPrefix(l, "a@x.com") {
			t.Errorf("renewed a sealed account: %q", w.logins)
		}
	}
	for _, s := range []string{"a@x.com                      login kept in the Keychain", "security unlock-keychain"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}

	_, w2 := renewFixture(t)
	w2.sealed, w2.keychain["a@x.com"] = true, true
	out.Reset()
	if code := runLoginTo(&out, io.Discard, cfg, []string{"a@x.com"}, w2.deps()); code != 1 || len(w2.logins) != 0 {
		t.Errorf("named sealed account: exit %d, logins %q, want 1 and none", code, w2.logins)
	}
	// An unsealed Keychain reads normally: the item alone refuses nothing.
	_, w3 := renewFixture(t)
	w3.keychain["a@x.com"] = true
	if code := runLoginTo(io.Discard, io.Discard, cfg, []string{"a@x.com"}, w3.deps()); code != 0 || len(w3.logins) != 1 {
		t.Errorf("unsealed: exit %d, logins %q", code, w3.logins)
	}
}

// A remote user cannot see this machine's screen: the login is asked for a
// remote page, and the instructions say to open the printed URL and paste the
// code. That nothing opens is pinned through the binary.
func TestLoginRemoteAsksForThePastedCode(t *testing.T) {
	cfg, w := renewFixture(t)
	w.remote = true
	var out bytes.Buffer
	if code := runLoginTo(&out, io.Discard, cfg, []string{"a@x.com"}, w.deps()); code != 0 {
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

// A live session refreshing the access token while the batch waits on other
// approvals changes the credential without any login. Only a new refresh
// expiry is a new login, and a failed vendor command is a failure whatever
// changed meanwhile. (review r1)
func TestLoginReadBackIgnoresAnAccessRefresh(t *testing.T) {
	cfg, w := renewFixture(t)
	d := w.deps()
	d.login = func(a accounts.Account, _ accounts.Set, _ string, _ approvalPage) error {
		// another session refreshed the access token; the login was abandoned
		w.raw[a.Name] = blobEnding("refreshed-"+a.Name, w.now.Add(3*24*time.Hour))
		return fmt.Errorf("exit status 1")
	}
	var out bytes.Buffer
	if code := runLoginTo(&out, io.Discard, cfg, []string{"a@x.com"}, d); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ a@x.com: claude auth login: exit status 1") {
		t.Errorf("vendor failure not reported:\n%s", out.String())
	}
	d.login = func(a accounts.Account, _ accounts.Set, _ string, _ approvalPage) error {
		w.raw[a.Name] = blobEnding("refreshed-again-"+a.Name, w.now.Add(3*24*time.Hour))
		return nil
	}
	out.Reset()
	if code := runLoginTo(&out, io.Discard, cfg, []string{"a@x.com"}, d); code != 1 || !strings.Contains(out.String(), "✗ a@x.com: the stored login did not change") {
		t.Errorf("access-only refresh read as a login: exit %d\n%s", code, out.String())
	}
}

// An empty name is a caller's expansion bug, not a request for the pinned
// account. (review r1)
func TestLoginRefusesAnEmptyName(t *testing.T) {
	cfg, w := renewFixture(t)
	var errw bytes.Buffer
	if code := runLoginTo(io.Discard, &errw, cfg, []string{""}, w.deps()); code != 2 {
		t.Errorf("exit %d, want 2 (%s)", code, errw.String())
	}
	if len(w.logins) != 0 {
		t.Errorf("an empty name logged in: %q", w.logins)
	}
}

// A primary with a login but no readable identity cannot have its new login
// checked, so it is held back — unless the vendor's own status names it.
// (review r1)
func TestLoginHoldsAPrimaryWhoseIdentityIsUnknown(t *testing.T) {
	cfg, w := renewFixture(t)
	w.raw["primary"] = blobEnding("old-p", w.now.Add(24*time.Hour))
	var out bytes.Buffer
	if code := runLoginTo(&out, io.Discard, cfg, []string{"primary"}, w.deps()); code != 1 {
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
	if code := runLoginTo(&out, io.Discard, cfg, []string{"primary"}, d); code != 1 || !strings.Contains(out.String(), "logged in as a@x.com, not p@x.com") {
		t.Errorf("primary not checked against the vendor's email: exit %d\n%s", code, out.String())
	}
}
