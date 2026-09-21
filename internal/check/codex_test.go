package check

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
)

// stubCodex answers `login status` the way 0.155.0 does: "Logged in using
// ChatGPT" on stdout, or "Not logged in" on stderr with exit 1, decided by the
// home CODEX_HOME selects. keyringHome, when set, is a home that reports a
// login with no auth.json — the keyring credential store. The seams sit in a
// comment, as they do in the claude stub. It proves the probes' wiring and
// verdict mapping, never that the installed binary honours CODEX_HOME.
func stubCodex(t *testing.T, bin, primaryHome, keyringHome string, loginOut string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
# wham/usage ChatGPT-Account-Id CODEX_HOME CODEX_ACCESS_TOKEN
home="${CODEX_HOME:-%s}"
if [ -n "$CODEX_API_KEY$CODEX_ACCESS_TOKEN" ]; then echo "leaked credential variable"; exit 3; fi
if [ "$home" = %q ] || [ -f "$home/auth.json" ]; then %s; exit 0; fi
echo "Not logged in" >&2
exit 1
`, primaryHome, keyringHome, loginOut)
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

type codexRun struct {
	lines             []string
	fails, own, skips int
}

func (r *codexRun) has(sub string) bool {
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func runCodexCheck(scope config.Scope, environ []string) *codexRun {
	r := &codexRun{}
	chk := func(ok bool, label, hint string) {
		if !ok {
			r.fails++
			r.lines = append(r.lines, "FAIL "+label+" — "+hint)
			return
		}
		r.lines = append(r.lines, "ok "+label)
	}
	own := func(ok bool, label, hint string) {
		if !ok {
			r.own++
		}
		chk(ok, label, hint)
	}
	skip := func(label, why string) {
		r.skips++
		r.lines = append(r.lines, "?? "+label+" — "+why)
	}
	checkCodex(scope, environ, chk, own, skip)
	return r
}

func codexCheckFixture(t *testing.T, usageURL string) (config.Scope, string) {
	t.Helper()
	cfg := config.ForHome(t.TempDir())
	scope := cfg.Codex
	scope.Present, scope.UsageURL = true, usageURL
	if err := os.MkdirAll(scope.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	return scope, bin
}

func seedCodexExtra(t *testing.T, scope config.Scope, l codexauthtest.Login) string {
	t.Helper()
	dir := filepath.Join(scope.AccountsRoot, l.Email)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scope.StoreDir(), filepath.Join(dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	if l.Token != "" {
		l.Write(t, dir)
	}
	return dir
}

const checkBody = `{"account_id":"acct-1","user_id":"user-1","rate_limit":{"allowed":true,"limit_reached":false,
  "primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_after_seconds":10,"reset_at":1790239759}}}`

var checkLogin = codexauthtest.Login{Email: "a@x.com", Plan: "pro", AccountID: "acct-1", UserID: "user-1", Token: "t1"}

// Obligation 14: PASS for a sound tree; FAIL on a drifted usage document;
// INCONCLUSIVE on 429, transport failure, a stale token — and on any 401,
// because Codex recovers from a 401 by refreshing.
func TestCodexCheckRequestVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		stale, dead  bool
		fails, skips int
		phrase       string
	}{
		{"sound", 200, checkBody, false, false, 0, 0, "api[a@x.com]: HTTP 200, 1 row(s), fields well-formed"},
		// A degraded body is still stored, so the live line and the audit of
		// what was stored both fail on it.
		{"drifted usage field", 200, strings.Replace(checkBody, `"used_percent":4`, `"used_percent":"4"`, 1), false, false, 2, 0, "malformed field"},
		{"drifted allowance", 200, strings.Replace(checkBody, `"allowed":true`, `"allowed":"yes"`, 1), false, false, 2, 0, "malformed field"},
		{"drifted envelope", 200, `{"limits":[]}`, false, false, 1, 0, "unparseable body"},
		{"a body naming someone else", 200, strings.Replace(checkBody, "acct-1", "acct-9", 1), false, false, 1, 0, "unparseable body"},
		{"rate limited", 429, ``, false, false, 0, 1, "rate limited"},
		{"unauthorized", 401, ``, false, false, 0, 1, "HTTP 401"},
		{"vendor error", 503, ``, false, false, 0, 1, "vendor-side"},
		{"transport", 0, ``, false, true, 0, 1, "transport error"},
		{"stale token", 200, checkBody, true, false, 0, 1, "access token stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stale {
					t.Error("a request left on a stale token")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			url := srv.URL
			if tc.dead {
				srv.Close()
			} else {
				defer srv.Close()
			}
			scope, bin := codexCheckFixture(t, url)
			stubCodex(t, bin, scope.PrimaryDir(), "", `echo "Logged in using ChatGPT"`)
			l := checkLogin
			if tc.stale {
				l.Expires = time.Now().Add(-time.Hour)
			}
			seedCodexExtra(t, scope, l)

			r := runCodexCheck(scope, []string{"PATH=" + bin})
			if r.fails != tc.fails || r.skips != tc.skips || !r.has(tc.phrase) {
				t.Errorf("fails=%d skips=%d, want %d/%d with %q\n%s", r.fails, r.skips, tc.fails, tc.skips, tc.phrase, strings.Join(r.lines, "\n"))
			}
			if r.own != 0 {
				t.Errorf("a vendor finding was reported against headroom's own files:\n%s", strings.Join(r.lines, "\n"))
			}
			if tc.name == "sound" {
				for _, want := range []string{"binary (codex)", "auth[a@x.com]", "isolation:", "credentials[a@x.com]", "credentials[primary]", "topology[a@x.com]: sessions/", "current:"} {
					if !r.has("ok " + want) {
						t.Errorf("sound tree is missing %q:\n%s", want, strings.Join(r.lines, "\n"))
					}
				}
			}
		})
	}
}

// A drifted auth document fails, per document row.
func TestCodexCheckAuthDrift(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":           `nope`,
		"no access token":    `{"auth_mode":"chatgpt","tokens":{"account_id":"acct-1"}}`,
		"identity undecoded": string(codexauthtest.Login{Email: "a@x.com", AccountID: "acct-1", Token: "t"}.Document()),
	} {
		t.Run(name, func(t *testing.T) {
			scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
			stubCodex(t, bin, scope.PrimaryDir(), "", `echo "Logged in using ChatGPT"`)
			dir := seedCodexExtra(t, scope, codexauthtest.Login{Email: "a@x.com"})
			codexauthtest.WriteRaw(t, dir, []byte(doc))
			r := runCodexCheck(scope, []string{"PATH=" + bin})
			if r.fails == 0 || !r.has("FAIL auth[a@x.com]") {
				t.Errorf("drifted auth.json did not fail:\n%s", strings.Join(r.lines, "\n"))
			}
		})
	}
	// Another auth mode is a fact, not drift.
	scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
	stubCodex(t, bin, scope.PrimaryDir(), "", `echo "Logged in using an API key"`)
	dir := seedCodexExtra(t, scope, codexauthtest.Login{Email: "k@x.com"})
	codexauthtest.WriteRaw(t, dir, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk"}`))
	if r := runCodexCheck(scope, []string{"PATH=" + bin}); r.fails != 0 || !r.has(`auth_mode "apikey"`) {
		t.Errorf("api key login:\n%s", strings.Join(r.lines, "\n"))
	}
}

// The credential-source probe, in both directions, and the isolation probe.
func TestCodexCheckCredentialSource(t *testing.T) {
	t.Run("logged in beside no auth.json is undetermined", func(t *testing.T) {
		scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
		keyring := seedCodexExtra(t, scope, codexauthtest.Login{Email: "ring@x.com"})
		stubCodex(t, bin, scope.PrimaryDir(), keyring, `echo "Logged in using ChatGPT"`)
		r := runCodexCheck(scope, []string{"PATH=" + bin})
		if r.fails != 0 || !r.has("?? credentials[ring@x.com]") || !r.has("credential source undetermined") {
			t.Errorf("keyring home:\n%s", strings.Join(r.lines, "\n"))
		}
	})
	t.Run("not logged in beside an accepted auth.json is drift", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(checkBody)) }))
		defer srv.Close()
		scope, bin := codexCheckFixture(t, srv.URL)
		seedCodexExtra(t, scope, checkLogin)
		// A stub that never sees a login, whatever the home holds.
		if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n# wham/usage ChatGPT-Account-Id CODEX_HOME CODEX_ACCESS_TOKEN\necho 'Not logged in' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runCodexCheck(scope, []string{"PATH=" + bin})
		if !r.has("FAIL credentials[a@x.com]") {
			t.Errorf("disagreement did not fail:\n%s", strings.Join(r.lines, "\n"))
		}
	})
	t.Run("an empty home that reports a login fails loudly", func(t *testing.T) {
		scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
		if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n# wham/usage ChatGPT-Account-Id CODEX_HOME CODEX_ACCESS_TOKEN\necho 'Logged in using ChatGPT'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runCodexCheck(scope, []string{"PATH=" + bin})
		if !r.has("FAIL isolation:") {
			t.Errorf("broken isolation did not fail:\n%s", strings.Join(r.lines, "\n"))
		}
	})
	t.Run("a command failure is not an answer", func(t *testing.T) {
		scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
		if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n# wham/usage ChatGPT-Account-Id CODEX_HOME CODEX_ACCESS_TOKEN\necho 'error: unexpected argument' >&2\nexit 2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runCodexCheck(scope, []string{"PATH=" + bin})
		if r.fails != 0 || !r.has("?? isolation: not tested") || !r.has("?? credentials[primary]: not tested") {
			t.Errorf("command failure:\n%s", strings.Join(r.lines, "\n"))
		}
	})
	t.Run("the probe strips inherited credential variables", func(t *testing.T) {
		scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
		stubCodex(t, bin, scope.PrimaryDir(), "", `echo "Logged in using ChatGPT"`)
		r := runCodexCheck(scope, []string{"PATH=" + bin, "CODEX_ACCESS_TOKEN=tok-secret", "CODEX_HOME=/somewhere/else"})
		if !r.has("ok isolation:") {
			t.Errorf("an inherited credential reached the probe:\n%s", strings.Join(r.lines, "\n"))
		}
		for _, l := range r.lines {
			if strings.Contains(l, "tok-secret") {
				t.Errorf("a credential value was printed: %s", l)
			}
		}
		if !r.has("CODEX_ACCESS_TOKEN (value not shown)") || !r.has("CODEX_HOME is neutralized") {
			t.Errorf("inherited variables not reported:\n%s", strings.Join(r.lines, "\n"))
		}
	})
}

// A corrupt Codex state.json reports as headroom's own file, never as Codex
// having changed a format.
func TestCodexCheckCorruptStateIsOwn(t *testing.T) {
	scope, bin := codexCheckFixture(t, "http://127.0.0.1:1")
	stubCodex(t, bin, scope.PrimaryDir(), "", `echo "Logged in using ChatGPT"`)
	if err := os.MkdirAll(scope.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope.AccountsRoot, "state.json"), []byte(`{"version":1,"accounts":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCodexCheck(scope, []string{"PATH=" + bin})
	if r.fails == 0 || r.fails != r.own {
		t.Errorf("fails=%d own=%d:\n%s", r.fails, r.own, strings.Join(r.lines, "\n"))
	}
}

// A machine without Codex: one informational line, the exit code unchanged.
// With Codex present, its lines carry the vendor's prefix and the closing line
// blames the right vendor.
func TestRunCodexPresence(t *testing.T) {
	home := t.TempDir()
	cfg := config.ForHome(home)
	t.Setenv("PATH", t.TempDir())
	var out strings.Builder
	Run(cfg, &out, false)
	if !strings.Contains(out.String(), "codex: not found") || strings.Contains(out.String(), "codex auth[") {
		t.Errorf("absent Codex:\n%s", out.String())
	}

	cfg.Codex.Present = true
	dir := filepath.Join(cfg.Codex.AccountsRoot, "a@x.com")
	codexauthtest.WriteRaw(t, dir, []byte(`nope`))
	out.Reset()
	Run(cfg, &out, false)
	if !strings.Contains(out.String(), "codex auth[a@x.com]") || !strings.Contains(out.String(), "Codex likely changed a format") {
		t.Errorf("present Codex:\n%s", out.String())
	}
}
