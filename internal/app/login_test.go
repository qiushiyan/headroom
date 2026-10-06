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
		readRaw:   func(a accounts.Account) string { return w.raw[a.Name] },
		metaEmail: func(a accounts.Account) string { return w.email[a.Name] },
		profiles:  []browser.Profile{{Dir: "Profile 2", Name: "a"}},
		now:       func() time.Time { return w.now },
		login: func(a accounts.Account, _ accounts.Set, email, profileDir string) error {
			w.logins = append(w.logins, a.Name+" "+email+" "+profileDir)
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
	for _, s := range []string{"Chrome profile a (Profile 2)", "default browser (no Chrome profile matches c@x.com)", "✓ a@x.com: logged in, login ends Nov 5"} {
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

// A login that ran but stored nothing new did not take, whatever the exit.
func TestLoginReadBackCatchesUnchangedCredential(t *testing.T) {
	cfg, w := renewFixture(t)
	d := w.deps()
	d.login = func(accounts.Account, accounts.Set, string, string) error { return nil }
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
