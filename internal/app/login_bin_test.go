package app

import (
	"fmt"
	"io"
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
// does: exec $BROWSER with the URL, then store a new login in its dir — after
// reading the code from its stdin when PASTE is set.
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
	binLogin(t, primary, "p@x.com", time.Now().Add(25*24*time.Hour))
	binLogin(t, h.extra, "a@x.com", time.Now().Add(3*24*time.Hour))
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
  echo browsed >> "$RECORD"
  if [ -n "$PASTE" ]; then read -r code; echo "code=$code" >> "$RECORD"; fi
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

// binLogin stores a login in dir, as email, ending at end.
func binLogin(t *testing.T, dir, email string, end time.Time) {
	t.Helper()
	writeJSON(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, email, email))
	writeJSON(t, filepath.Join(dir, ".credentials.json"), fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli(), end.UnixMilli()))
}

// addExtra adds an extra account beside a@x.com, logged in until end.
func (h loginBin) addExtra(t *testing.T, name string, end time.Time) {
	t.Helper()
	dir := filepath.Join(h.home, ".claude-accounts", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.home, ".claude", "projects"), filepath.Join(dir, "projects")); err != nil {
		t.Fatal(err)
	}
	binLogin(t, dir, name, end)
}

// start runs the binary with its stdin on a pipe the test pastes into, as a
// person at the terminal would, remote, with the vendor stub reading a code.
func (h loginBin) start(t *testing.T, args ...string) (*exec.Cmd, io.WriteCloser, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(headroomBinary(t), args...)
	cmd.Env = []string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"RECORD=" + h.record, "PASTE=1",
		"SSH_CONNECTION=10.0.0.1 50000 10.0.0.2 22",
	}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, stdin, out
}

// await polls until ready, and ends cmd and the test if it never is.
func await(t *testing.T, cmd *exec.Cmd, what string, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ready(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("%s never happened", what)
		}
	}
}

// browsed is how many approval pages the vendor stub has opened.
func (h loginBin) browsed() int {
	rec, _ := os.ReadFile(h.record)
	return strings.Count(string(rec), "browsed\n")
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
	if strings.Contains(rec, "open ") || !strings.Contains(rec, "browsed") {
		t.Errorf("a page was opened for a remote user:\n%s", rec)
	}
}

// The code reaches the vendor through headroom, which learned the sign-in's
// state from the URL the vendor handed its $BROWSER: a code another sign-in
// issued is held back, and this sign-in's goes through.
func TestLoginBinaryRelaysOnlyThisSigninsCode(t *testing.T) {
	h := newLoginBin(t)
	cmd, stdin, out := h.start(t, "login")
	// Paste only once the page is up, as a person would.
	await(t, cmd, "the vendor stub opening its page", func() bool { return h.browsed() == 1 })
	fmt.Fprint(stdin, "stale#other\ngood#s\n")
	stdin.Close()
	cmd.Wait()
	rec, _ := os.ReadFile(h.record)
	if !strings.Contains(string(rec), "code=good#s") || strings.Contains(string(rec), "code=stale") {
		t.Errorf("vendor got the wrong code:\n%s\noutput:\n%s", rec, out.String())
	}
	if !strings.Contains(out.String(), "✓ a@x.com: logged in") {
		t.Errorf("the login did not take:\n%s", out.String())
	}
}

// A line pasted after one login ended and before the next began was meant
// for the one that ended: the next login drops it rather than hand it to its
// vendor. The pause between the two is made by the Keychain read that reads
// the first login back, which the stub holds until the stray line is in.
// Deleting the drainLines call in vendorLogin must fail this test.
func TestLoginBinaryDropsWhatWasPastedBetweenLogins(t *testing.T) {
	h := newLoginBin(t)
	h.addExtra(t, "b@x.com", time.Now().Add(3*24*time.Hour))
	security := `#!/bin/sh
if [ "$1" = find-generic-password ] && grep -q '^code=' "$RECORD" && [ ! -e "$RECORD.held" ]; then
  : > "$RECORD.held"
  i=0; while [ ! -e "$RECORD.pasted" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
  sleep 0.3
fi
exit 44
`
	if err := os.WriteFile(filepath.Join(h.stubs, "security"), []byte(security), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, stdin, out := h.start(t, "login")
	await(t, cmd, "a@x.com's page", func() bool { return h.browsed() == 1 })
	fmt.Fprint(stdin, "first#s\n")
	await(t, cmd, "a@x.com's read-back", func() bool { _, err := os.Stat(h.record + ".held"); return err == nil })
	fmt.Fprint(stdin, "stray#s\n")
	if err := os.WriteFile(h.record+".pasted", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	await(t, cmd, "b@x.com's page", func() bool { return h.browsed() == 2 })
	fmt.Fprint(stdin, "second#s\n")
	stdin.Close()
	cmd.Wait()
	rec, _ := os.ReadFile(h.record)
	var codes []string
	for line := range strings.SplitSeq(string(rec), "\n") {
		if c, ok := strings.CutPrefix(line, "code="); ok {
			codes = append(codes, c)
		}
	}
	if strings.Join(codes, " ") != "first#s second#s" {
		t.Errorf("the vendors read %q, want first#s then second#s\noutput:\n%s", codes, out.String())
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
