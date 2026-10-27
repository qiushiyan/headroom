package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/signin"
)

// handoffBin is a home for the real binary's handed-off sign-in, short
// enough for its socket: one extra whose login ends in three days, and a
// vendor stub that prints Claude Code 2.1.311's login screen on a pipe,
// hands $BROWSER the localhost URL of the same sign-in, waits for a code on
// its input — through an input at end of file — and stores a new login for
// good#stub-state alone.
type handoffBin struct {
	home, extra, stubs, record, bin string
}

func newHandoffBin(t *testing.T) handoffBin {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "hrb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	h := handoffBin{home: filepath.Join(base, "h"), stubs: filepath.Join(base, "s"), record: filepath.Join(base, "r"), bin: headroomBinary(t)}
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
	newEnd := time.Now().Add(29 * 24 * time.Hour).UnixMilli()
	printed := signinURL("stub-state")
	opened := strings.Replace(printed, "https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback", "http%3A%2F%2Flocalhost%3A4000%2Fcallback", 1)
	for name, body := range map[string]string{
		"claude": `#!/bin/sh
case "$1 $2" in
"auth status") echo '{"loggedIn":true}' ;;
"auth login")
  echo "login $* pid=$$" >> "$RECORD"
  echo "Opening browser to sign in…"
  echo "If the browser didn't open, visit: ` + printed + `"
  "$BROWSER" "` + opened + `"
  printf 'Paste code here if prompted > '
  # As Claude Code does, an input at end of file does not end the wait.
  read -r code || exec sleep 600
  echo "code=$code" >> "$RECORD"
  if [ "$code" != "good#stub-state" ]; then echo "Login failed: That code wasn't accepted."; exit 1; fi
  printf '{"claudeAiOauth":{"accessToken":"new","expiresAt":1,"refreshTokenExpiresAt":` + fmt.Sprint(newEnd) + `}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
  echo "Login successful." ;;
esac
`,
		"open":     "#!/bin/sh\necho \"open $*\" >> \"$RECORD\"\n",
		"security": "#!/bin/sh\nexit 44\n",
	} {
		if err := os.WriteFile(filepath.Join(h.stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { h.run(t, "", "login", "cancel", "a@x.com") })
	return h
}

func (h handoffBin) env() []string {
	return []string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"RECORD=" + h.record,
	}
}

// run is the binary as sshd runs a command: its streams pipes read to their
// end, so a holder that kept one would hold the call open.
func (h handoffBin) run(t *testing.T, stdin string, args ...string) (signinDoc, int, time.Duration) {
	t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Env = h.env()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.MultiReader(out, errp))
		read <- b
	}()
	var got []byte
	select {
	case got = <-read:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("%q: its streams were still open after 20s", args)
	}
	took := time.Since(began)
	cmd.Wait()
	var doc signinDoc
	json.Unmarshal(got, &doc)
	return doc, cmd.ProcessState.ExitCode(), took
}

func (h handoffBin) recordOf(t *testing.T) signin.Record {
	t.Helper()
	rec, _, err := signin.Read(signin.For(filepath.Join(h.home, ".claude-accounts"), h.extra).Record())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func alive(p signin.Proc) bool {
	start, err := sessions.PS(p.PID)
	return p.PID > 0 && err == nil && start == p.Start
}

// Through the process boundary: start returns, every stream it was given at
// end of file, while a holder in a session of its own keeps the vendor
// waiting; nothing opened; finish renews, and the holder and vendor are
// gone after. (login-handoff spec, test plan 5)
func TestHandoffBinaryStartReturnsAndFinishRenews(t *testing.T) {
	h := newHandoffBin(t)
	doc, code, took := h.run(t, "", "login", "start", "a@x.com")
	if code != 0 || doc.State != signin.Awaiting || doc.URL != signinURL("stub-state") {
		t.Fatalf("start: exit %d, %+v", code, doc)
	}
	if took > 10*time.Second {
		t.Errorf("start took %s", took)
	}
	rec := h.recordOf(t)
	if !alive(rec.Holder) || !alive(rec.Vendor) {
		t.Fatalf("holder %v or vendor %v not running after start returned", rec.Holder, rec.Vendor)
	}
	if sid, _ := syscall.Getsid(rec.Holder.PID); sid != rec.Holder.PID {
		t.Errorf("the holder leads no session of its own (sid %d)", sid)
	}
	doc, code, _ = h.run(t, "bad#stub-other\n", "login", "finish", "a@x.com")
	if code != 3 || doc.reason() != reasonOtherSignin {
		t.Errorf("another sign-in's code: exit %d, %+v", code, doc)
	}
	doc, code, _ = h.run(t, "good#stub-state\n", "login", "finish", "a@x.com")
	if code != 0 || doc.State != signin.Renewed || doc.LoginEndsAt == nil {
		t.Fatalf("finish: exit %d, %+v", code, doc)
	}
	logged, _ := os.ReadFile(h.record)
	if strings.Count(string(logged), "code=") != 1 || strings.Contains(string(logged), "open ") {
		t.Errorf("the vendor's record:\n%s", logged)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(rec.Holder) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if alive(rec.Holder) || alive(rec.Vendor) {
		t.Errorf("holder or vendor still running after the sign-in ended")
	}
}

// A holder killed outright leaves its vendor waiting (measured, 2.1.311):
// finish says the sign-in ended before any code was sent, and the next start
// ends that vendor and begins afresh. (test plan 13)
func TestHandoffBinaryHolderKilled(t *testing.T) {
	h := newHandoffBin(t)
	if _, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 {
		t.Fatalf("start: exit %d", code)
	}
	first := h.recordOf(t)
	syscall.Kill(first.Holder.PID, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); alive(first.Holder) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if !alive(first.Vendor) {
		t.Fatal("the vendor ended with its holder; the orphan case did not arise")
	}
	if doc, code, _ := h.run(t, "good#stub-state\n", "login", "finish", "a@x.com"); code != 1 || doc.reason() != reasonHolderGone {
		t.Errorf("finish: exit %d, %+v", code, doc)
	}
	if doc, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 || doc.State != signin.Awaiting {
		t.Errorf("start again: exit %d, %+v", code, doc)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(first.Vendor) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if alive(first.Vendor) {
		t.Error("the orphaned vendor is still running")
	}
	if second := h.recordOf(t); !alive(second.Vendor) || second.Vendor.PID == first.Vendor.PID {
		t.Errorf("the new sign-in's vendor: %+v", second.Vendor)
	}
}

// `login hold` run by hand, without the lock start hands over, refuses.
func TestHandoffBinaryHoldNeedsTheLock(t *testing.T) {
	h := newHandoffBin(t)
	cmd := exec.Command(h.bin, "login", "hold", "a@x.com")
	cmd.Env = h.env()
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "run by headroom login start") {
		t.Errorf("hold by hand: %v\n%s", err, out)
	}
}
