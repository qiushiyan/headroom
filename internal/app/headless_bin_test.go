package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/headless"
)

// Through the built binary, with a terminal on stdin (script(1) gives it
// one) and a vendor stub that does what `claude setup-token` does with its
// output on a pipe: draw its frames, exec $BROWSER with the URL, print the
// token. The token is stored readable by the user alone and never reaches
// the output; the vendor ran under a scratch config dir, since gone, not the
// account's, with no inherited credential; and the page opened in the
// account's profile.
func TestMintBinaryRunsTheVendorInAScratchDir(t *testing.T) {
	h := newLoginBin(t)
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("no script(1) to give the binary a terminal")
	}
	stub := `#!/bin/sh
case "$1" in
setup-token)
  { echo "args $*"; echo "dir=$CLAUDE_CONFIG_DIR"; echo "browser=$BROWSER"; echo "oauth=${CLAUDE_CODE_OAUTH_TOKEN:-unset}"
    if [ -t 1 ]; then echo "stdout=tty"; else echo "stdout=pipe"; fi; } >> "$RECORD"
  echo "Welcome to Claude Code v2.1.296"
  echo " This will guide you through long-lived (1-year) auth token setup for your Claude account."
  "$BROWSER" "https://claude.com/cai/oauth/authorize?scope=user%3Ainference&state=s"
  echo '{}' > "$CLAUDE_CONFIG_DIR/.claude.json"
  printf ' Your OAuth token (valid for 1 year):\n\n ` + testToken + `\n\n Store this token securely.\n' ;;
*) exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(h.stubs, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := headroomBinary(t)
	cmd := exec.Command("script", "-q", "/dev/null", bin, "headless", "mint", "a@x.com")
	// A sign-in file named by the environment the mint was started in is
	// some other run's: the opener the vendor runs records in the mint's own.
	inherited := filepath.Join(t.TempDir(), "inherited-signin")
	cmd.Env = []string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"), "TERM=dumb",
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin", "RECORD=" + h.record,
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-inheritedinheritedinheritedinherited",
		"CLAUDE_CONFIG_DIR=" + h.extra,
		"HEADROOM_BROWSER_SIGNIN=" + inherited,
	}
	out, err := cmd.CombinedOutput()
	rec, _ := os.ReadFile(h.record)
	if err != nil || !strings.Contains(string(out), "✓ a@x.com: headless token stored") {
		t.Fatalf("mint: %v\n%s\nrecord:\n%s", err, out, rec)
	}
	if strings.Contains(string(out), testToken) || !strings.Contains(string(out), headless.Withheld) {
		t.Fatalf("the token reached the terminal:\n%s", out)
	}
	record := string(rec)
	dir := ""
	for line := range strings.SplitSeq(record, "\n") {
		if v, ok := strings.CutPrefix(line, "dir="); ok {
			dir = v
		}
	}
	if dir == "" || dir == h.extra || strings.HasPrefix(dir, h.home) || !filepath.IsAbs(dir) {
		t.Errorf("the vendor ran under %q, not a scratch dir", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the scratch dir %s is still there (%v)", dir, err)
	}
	if _, err := os.Stat(inherited); !os.IsNotExist(err) {
		t.Errorf("the opener recorded the sign-in in the inherited %s (%v)", inherited, err)
	}
	for _, want := range []string{
		"args setup-token", "oauth=unset", "stdout=pipe",
		"open -na Google Chrome --args --profile-directory=Profile 7 https://claude.com/cai/oauth/authorize?scope=user%3Ainference&state=s",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("record lacks %q:\n%s", want, record)
		}
	}
	root := filepath.Join(h.home, ".claude-accounts")
	info, err := os.Stat(headless.Path(root))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", info, err)
	}
	tokens, err := headless.Read(root)
	if err != nil || tokens["a@x.com"].Value != testToken || tokens["a@x.com"].Key != "uuid:u-a@x.com" || tokens["a@x.com"].EndsMS == 0 {
		t.Fatalf("stored %+v %v", tokens, err)
	}
	// The account's own login is as it was.
	if cred, _ := os.ReadFile(filepath.Join(h.extra, ".credentials.json")); !strings.Contains(string(cred), `"accessToken":"old"`) {
		t.Errorf("the account's login changed: %s", cred)
	}

	// And it comes out only by name.
	tok := exec.Command(bin, "headless", "token", "a@x.com")
	tok.Env = cmd.Env
	got, err := tok.Output()
	if err != nil || string(got) != testToken+"\n" {
		t.Fatalf("token: %q %v", got, err)
	}
}
