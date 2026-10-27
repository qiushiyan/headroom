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
	extraEnv                        []string
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
  # A write to a reader that died fails without ending the vendor: whether
  # Claude Code's does is unmeasured, and the case under test is the one
  # where it survives.
  trap '' PIPE
  [ -n "$SLOW" ] && sleep "$SLOW"
  echo "Opening browser to sign in…"
  echo "If the browser didn't open, visit: ` + printed + `"
  "$BROWSER" "` + opened + `"
  printf 'Paste code here if prompted > '
  [ -n "$NOISE" ] && seq 1 "$NOISE"
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
	return append([]string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"RECORD=" + h.record,
	}, h.extraEnv...)
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
	rec, _, err := signin.Read(h.paths().Record())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (h handoffBin) paths() signin.Paths {
	return signin.For(filepath.Join(h.home, ".claude-accounts"), h.extra)
}

// vendorOf is the vendor login registered for the extra's dir.
func (h handoffBin) vendorOf() signin.Proc {
	p, _ := signin.ReadVendor(h.paths().Vendor())
	return p
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
	rec, vendor := h.recordOf(t), h.vendorOf()
	if !alive(rec.Holder) || !alive(vendor) {
		t.Fatalf("holder %v or vendor %v not running after start returned", rec.Holder, vendor)
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
	if alive(rec.Holder) || alive(vendor) {
		t.Errorf("holder or vendor still running after the sign-in ended")
	}
	if _, registered := signin.ReadVendor(h.paths().Vendor()); registered {
		t.Error("the reaped vendor is still registered")
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
	first, firstVendor := h.recordOf(t), h.vendorOf()
	syscall.Kill(first.Holder.PID, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); alive(first.Holder) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if !alive(firstVendor) {
		t.Fatal("the vendor ended with its holder; the orphan case did not arise")
	}
	if doc, code, _ := h.run(t, "good#stub-state\n", "login", "finish", "a@x.com"); code != 1 || doc.reason() != reasonHolderGone {
		t.Errorf("finish: exit %d, %+v", code, doc)
	}
	if doc, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 || doc.State != signin.Awaiting {
		t.Errorf("start again: exit %d, %+v", code, doc)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(firstVendor) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if alive(firstVendor) {
		t.Error("the orphaned vendor is still running")
	}
	if second := h.vendorOf(); !alive(second) || second.PID == firstVendor.PID {
		t.Errorf("the new sign-in's vendor: %+v", second)
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

// vendorPIDs are the pids the vendor stub recorded, in launch order.
func (h handoffBin) vendorPIDs(t *testing.T) []int {
	t.Helper()
	logged, _ := os.ReadFile(h.record)
	var pids []int
	for line := range strings.SplitSeq(string(logged), "\n") {
		if _, after, ok := strings.Cut(line, " pid="); ok {
			var pid int
			fmt.Sscan(after, &pid)
			pids = append(pids, pid)
		}
	}
	return pids
}

func running(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func awaitGone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); running(pid) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	return !running(pid)
}

// The holder keeps the account's login lock for its whole life, however
// much its garbage collector runs: a lock released early lets a second
// start take a live sign-in for a dead one. (review r1 F1)
func TestHandoffBinaryHolderKeepsItsLock(t *testing.T) {
	h := newHandoffBin(t)
	h.extraEnv = []string{"GOGC=1", "NOISE=200000"}
	if doc, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 || doc.State != signin.Awaiting {
		t.Fatalf("start: exit %d, %+v", code, doc)
	}
	time.Sleep(2 * time.Second) // the holder reads the noise, and collects
	if doc, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 || doc.Reused == nil || !*doc.Reused {
		t.Errorf("a second start: exit %d, %+v", code, doc)
	}
	if pids := h.vendorPIDs(t); len(pids) != 1 {
		t.Errorf("%d vendors started, want 1", len(pids))
	}
}

// A vendor is the dir's from its launch, not from its URL: a holder killed
// before the vendor printed one leaves a vendor the next start ends, and so
// does a batch killed mid-login. (review r1 F3)
func TestHandoffBinaryEndsAnyVendorLeftBehind(t *testing.T) {
	h := newHandoffBin(t)
	h.extraEnv = []string{"SLOW=3"}
	starting := exec.Command(h.bin, "login", "start", "a@x.com")
	starting.Env = h.env()
	if err := starting.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(h.vendorPIDs(t)) == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	holder, _ := exec.Command("pgrep", "-f", h.bin+" login hold a@x.com").Output()
	var hp int
	fmt.Sscan(string(holder), &hp)
	if hp == 0 || len(h.vendorPIDs(t)) != 1 {
		t.Fatalf("holder %q, vendors %v", holder, h.vendorPIDs(t))
	}
	first := h.vendorPIDs(t)[0]
	syscall.Kill(hp, syscall.SIGKILL)
	// The start that waited on that holder still holds the lock, and ends
	// the vendor it registered before reporting the holder failed.
	if err := starting.Wait(); err == nil {
		t.Error("start reported success for a holder that died")
	}
	if !awaitGone(first) {
		t.Error("a vendor started before its URL was left running")
	}
	if doc := h.recordOf(t); doc.Reason != reasonHolderFailed {
		t.Errorf("record after: %+v", doc)
	}

	// A batch, killed while its vendor waits for a code.
	os.Remove(h.record)
	batch := exec.Command(h.bin, "login", "a@x.com")
	batch.Env = append(h.env(), "SSH_CONNECTION=10.0.0.1 50000 10.0.0.2 22")
	in, _ := batch.StdinPipe()
	defer in.Close()
	if err := batch.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(h.vendorPIDs(t)) == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	batch.Process.Kill()
	batch.Wait()
	orphan := h.vendorPIDs(t)[0]
	if !running(orphan) {
		t.Fatal("the batch's vendor ended with it; nothing was left behind")
	}
	if _, code, _ := h.run(t, "", "login", "start", "a@x.com"); code != 0 {
		t.Errorf("start after the batch: exit %d", code)
	}
	if !awaitGone(orphan) {
		t.Error("a killed batch's vendor was left running")
	}
}
