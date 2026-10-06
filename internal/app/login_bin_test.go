package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// loginBin is a home for the real binary: a primary whose login is far from
// ending, one extra whose login ends in three days, a Chrome profile named
// after the extra, and stubs for the vendor (`claude`), `open` and `security`
// that record what they were handed. The vendor stub does what Claude Code
// does: exec $BROWSER with the URL, then store a new login in its dir.
type loginBin struct {
	home, extra, stubs, record string
}

func newLoginBin(t *testing.T) loginBin {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the profile opener is open(1), macOS only")
	}
	base := t.TempDir()
	h := loginBin{home: filepath.Join(base, "home"), stubs: filepath.Join(base, "stubs"), record: filepath.Join(base, "record")}
	primary := filepath.Join(h.home, ".claude")
	h.extra = filepath.Join(h.home, ".claude-accounts", "a@x.com")
	for _, dir := range []string{filepath.Join(primary, "projects"), h.extra, h.stubs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(primary, "projects"), filepath.Join(h.extra, "projects")); err != nil {
		t.Fatal(err)
	}
	login := func(dir, email string, end time.Time) {
		writeJSON(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, email, email))
		writeJSON(t, filepath.Join(dir, ".credentials.json"), fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
			time.Now().Add(time.Hour).UnixMilli(), end.UnixMilli()))
	}
	login(primary, "p@x.com", time.Now().Add(25*24*time.Hour))
	login(h.extra, "a@x.com", time.Now().Add(3*24*time.Hour))
	chrome := filepath.Join(h.home, "Library", "Application Support", "Google", "Chrome")
	if err := os.MkdirAll(chrome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(chrome, "Local State"),
		`{"profile":{"info_cache":{"Default":{"name":"P","user_name":"p@x.com"},"Profile 7":{"name":"a","user_name":""}}}}`)

	newEnd := time.Now().Add(29 * 24 * time.Hour).UnixMilli()
	for name, body := range map[string]string{
		"claude": `#!/bin/sh
case "$1 $2" in
"auth status") echo '{"loggedIn":true}' ;;
"auth login")
  { echo "login $*"; echo "dir=$CLAUDE_CONFIG_DIR"; echo "browser=$BROWSER"; } >> "$RECORD"
  "$BROWSER" "https://claude.com/cai/oauth/authorize?state=s"
  printf '{"claudeAiOauth":{"accessToken":"new","expiresAt":1,"refreshTokenExpiresAt":` + fmt.Sprint(newEnd) + `}}' > "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/.credentials.json" ;;
esac
`,
		"open":     "#!/bin/sh\necho \"open $*\" >> \"$RECORD\"\n",
		"security": "#!/bin/sh\nexit 44\n",
	} {
		if err := os.WriteFile(filepath.Join(h.stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h loginBin) run(t *testing.T, bin string, extraEnv []string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append([]string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"RECORD=" + h.record,
	}, extraEnv...)
	out, _ := cmd.CombinedOutput()
	rec, _ := os.ReadFile(h.record)
	return string(out), string(rec)
}

// At the machine: the login runs in the extra's dir with headroom as the
// vendor's $BROWSER, and headroom opens the URL in the profile named after
// the account. (review r1)
func TestLoginBinaryOpensTheMatchedProfile(t *testing.T) {
	h := newLoginBin(t)
	bin := headroomBinary(t)
	out, rec := h.run(t, bin, nil, "login")
	if !strings.Contains(out, "✓ a@x.com: logged in") {
		t.Fatalf("login did not take:\n%s\nrecord:\n%s", out, rec)
	}
	for _, want := range []string{
		"login auth login --email a@x.com",
		"dir=" + h.extra,
		"open -na Google Chrome --args --profile-directory=Profile 7 https://claude.com/cai/oauth/authorize?state=s",
	} {
		if !strings.Contains(rec, want) {
			t.Errorf("record lacks %q:\n%s", want, rec)
		}
	}
	if strings.Contains(rec, "dir=\n") || strings.Count(rec, "login auth login") != 1 {
		t.Errorf("the primary, not due, was logged in:\n%s", rec)
	}
	browserLine := ""
	for line := range strings.SplitSeq(rec, "\n") {
		if v, ok := strings.CutPrefix(line, "browser="); ok {
			browserLine = v
		}
	}
	if self, _ := filepath.EvalSymlinks(bin); browserLine == "" || filepath.Base(browserLine) != filepath.Base(self) {
		t.Errorf("$BROWSER = %q, want this binary %q", browserLine, self)
	}
}

// From another machine: nothing opens on this one. (review r1)
func TestLoginBinaryOpensNothingForARemoteUser(t *testing.T) {
	h := newLoginBin(t)
	out, rec := h.run(t, headroomBinary(t), []string{"SSH_CONNECTION=10.0.0.1 50000 10.0.0.2 22"}, "login")
	if !strings.Contains(out, "✓ a@x.com: logged in") || !strings.Contains(out, "paste the code it shows") {
		t.Fatalf("remote login:\n%s\nrecord:\n%s", out, rec)
	}
	if strings.Contains(rec, "open ") || !strings.Contains(rec, "browser=/usr/bin/true") {
		t.Errorf("a page was opened for a remote user:\n%s", rec)
	}
}

// headroom as $BROWSER answers whatever configuration the vendor's
// environment carries: it dispatches before configuration is loaded.
func TestOpenerDispatchesBeforeConfiguration(t *testing.T) {
	h := newLoginBin(t)
	_, rec := h.run(t, headroomBinary(t), []string{"HEADROOM_BROWSER_PROFILE=Profile 7", "HEADROOM_HOME=relative/refused"}, "https://claude.com/x")
	if !strings.Contains(rec, "open -na Google Chrome --args --profile-directory=Profile 7 https://claude.com/x") {
		t.Errorf("opener did not run under a refused configuration:\n%s", rec)
	}
}
