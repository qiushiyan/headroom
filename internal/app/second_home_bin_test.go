package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// binHomes lays out, for the real binary, the owner's accounts root and a
// second home that has joined its ledger the way the steward's will: its own
// primary and account dirs, file-backed logins, and `.ledger` naming the
// owner's root. Nothing points HEADROOM_* anywhere — the binary finds the
// second home from HOME alone, as it will under launchd.
type binHomes struct {
	ownerRoot, home string
	steward         config.Scope // as the binary resolves it
	stubs           string
}

func newBinHomes(t *testing.T, access time.Time) binHomes {
	t.Helper()
	base := t.TempDir()
	h := binHomes{ownerRoot: filepath.Join(base, "qiushiyan", ".claude-accounts"), home: filepath.Join(base, ".steward-home")}
	h.steward = config.ForHome(h.home).Claude
	h.steward.PrimaryExplicit = true
	h.steward.LedgerRoot = h.ownerRoot
	for _, dir := range []string{h.ownerRoot, h.steward.StoreDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	far := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	login := func(dir, email string) {
		writeJSON(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, email, email))
		writeJSON(t, filepath.Join(dir, ".credentials.json"),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, email, access.UnixMilli(), far))
	}
	login(h.steward.PrimaryDir(), "yan@x.com")
	for _, email := range []string{"a@x.com", "b@x.com"} {
		dir := filepath.Join(h.steward.AccountsRoot, email)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		login(dir, email)
		if err := os.Symlink(h.steward.StoreDir(), filepath.Join(dir, "projects")); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(t, h.steward.LedgerFile(), h.ownerRoot+"\n")

	// claude records what it was started with; security finds no Keychain.
	h.stubs = t.TempDir()
	for name, body := range map[string]string{
		"claude":   "#!/bin/sh\n{ for a in \"$@\"; do echo \"arg $a\"; done; env | sed 's/^/env /'; } >> \"$RECORD\"\necho --- >> \"$RECORD\"\n",
		"security": "#!/bin/sh\nexit 44\n",
	} {
		if err := os.WriteFile(filepath.Join(h.stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// env is a launchd-sized environment for the second home: HOME, a PATH whose
// first entry holds its claude, the job's token, and nothing of the owner's
// shell — the CLAUDE_CONFIG_DIR the steward sets today included.
func (h binHomes) env(extra ...string) []string {
	return append([]string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"STEWARD_ATTEMPT=job-1",
		"CLAUDE_CONFIG_DIR=" + h.steward.PrimaryDir(),
	}, extra...)
}

// recorded is one start of the claude stub: its arguments and environment.
type recorded struct {
	args []string
	env  map[string]string
}

func readRecord(t *testing.T, path string) []recorded {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []recorded
	cur := recorded{env: map[string]string{}}
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case line == "---":
			out = append(out, cur)
			cur = recorded{env: map[string]string{}}
		case strings.HasPrefix(line, "arg "):
			cur.args = append(cur.args, strings.TrimPrefix(line, "arg "))
		case strings.HasPrefix(line, "env "):
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "env "), "=")
			cur.env[k] = v
		}
	}
	return out
}

// The steward's turns through the real binary, as envoy runs them: HOME at
// the second home, no terminal on any stream, the launcher prefix's --auto,
// and the provider arguments after it. The session starts on one of the
// second home's own dirs, its next turn continues there, its load lands in
// the owner's ledger and its routing in its own home.
func TestASecondHomeLaunchesWithoutATerminal(t *testing.T) {
	bin := headroomBinary(t)
	// Access tokens aged out: placement still uses every account, and no
	// refresh is left behind to outlive the test.
	h := newBinHomes(t, time.Now().Add(-time.Hour))
	record := filepath.Join(t.TempDir(), "record")
	const sid = "12345678-1234-4123-8123-123456789abc"
	provider := func(flag string) []string {
		return []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "claude-opus-5-5", "--effort", "high",
			flag, sid, "--permission-mode", "bypassPermissions", "--max-budget-usd", "60"}
	}
	turn := func(flag string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"launch", "--auto", "--"}, provider(flag)...)...)
		cmd.Env = h.env("RECORD="+record, "HEADROOM_USAGE_URL=http://127.0.0.1:1/usage")
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("launch %s: %v\n%s", flag, err, stderr.String())
		}
		return stderr.String()
	}

	first := turn("--session-id")
	second := turn("--resume")
	runs := readRecord(t, record)
	if len(runs) != 2 {
		t.Fatalf("claude started %d times", len(runs))
	}
	for i, r := range runs {
		want := provider([]string{"--session-id", "--resume"}[i])
		if !slices.Equal(r.args, want) {
			t.Errorf("turn %d: claude got %q, want the provider arguments intact", i+1, r.args)
		}
		dir := r.env["CLAUDE_CONFIG_DIR"]
		if !strings.HasPrefix(dir, h.home+"/") {
			t.Errorf("turn %d ran on %q, not a dir of the second home", i+1, dir)
		}
		if r.env["HOME"] != h.home || r.env["STEWARD_ATTEMPT"] != "job-1" {
			t.Errorf("turn %d: the rest of the environment did not pass through: HOME=%q STEWARD_ATTEMPT=%q", i+1, r.env["HOME"], r.env["STEWARD_ATTEMPT"])
		}
	}
	if runs[0].env["CLAUDE_CONFIG_DIR"] != runs[1].env["CLAUDE_CONFIG_DIR"] {
		t.Errorf("the session moved between turns: %s then %s", runs[0].env["CLAUDE_CONFIG_DIR"], runs[1].env["CLAUDE_CONFIG_DIR"])
	}
	if !strings.Contains(first, "headroom launch: ") || !strings.Contains(first, " · auto") || !strings.Contains(second, "this session's account") {
		t.Errorf("launch lines:\n%s\n%s", first, second)
	}

	owner := state.Open(config.Scope{Vendor: config.Claude, AccountsRoot: h.ownerRoot}).Load()
	recent := owner.Placements().Recent
	if len(recent) != 2 || recent[0].Home != h.steward.AccountsRoot {
		t.Errorf("the owner's ledger holds %+v, want both turns from the second home", recent)
	}
	if _, ok := owner.Owner(sid); ok {
		t.Error("the second home's session is routed in the owner's file")
	}
	if _, ok := state.Open(h.steward).Load().Owner(sid); !ok {
		t.Error("the second home did not record where its session went")
	}
	members := owner.Members()
	if len(members) != 1 || members[0].Root != h.steward.AccountsRoot || !members[0].Explicit {
		t.Errorf("registered homes = %+v", members)
	}
}

// What a launch leaves running is a refresh round, and the steward's
// supervisor ends it with its job: SIGTERM, then SIGKILL fifteen seconds
// later. Killed outright mid-request, it leaves the shared store readable,
// unlocked, and holding a claim that expires at its spacing. Asked to stop, it
// stops within a second and records the request it abandoned.
func TestARefreshKilledMidRequestLeavesTheStoreUsable(t *testing.T) {
	bin := headroomBinary(t)
	h := newBinHomes(t, time.Now().Add(time.Hour))
	asked := make(chan string, 8)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.Header.Get("Authorization")
		select {
		case <-release:
			w.Write([]byte(`{"limits":[]}`))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	st := state.Open(h.steward)
	keyOf := func(email string) state.Key { return state.Key{UUID: "u-" + email, Name: email} }
	// Every account but one is inside its quiet period, so each round asks
	// about exactly one.
	now := time.Now()
	quiet := func(emails ...string) {
		t.Helper()
		var keys []state.Key
		for _, e := range emails {
			keys = append(keys, keyOf(e))
		}
		if _, err := st.Claim(keys, now); err != nil {
			t.Fatal(err)
		}
	}
	quiet("yan@x.com", "b@x.com")

	start := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(bin, "refresh")
		cmd.Env = h.env("HEADROOM_USAGE_URL=" + srv.URL)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-asked:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("the refresh asked nothing")
		}
		return cmd
	}

	// SIGKILL mid-request.
	cmd := start()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	snap := st.Load()
	if p := snap.Problems(); len(p) != 0 {
		t.Fatalf("the store reads with problems after a kill: %+v", p)
	}
	next := snap.NextEligible(keyOf("a@x.com"), time.Now())
	if !next.After(time.Now()) || next.After(time.Now().Add(h.steward.RequestSpacing())) {
		t.Errorf("the killed request's claim stands until %v — want a deadline within one spacing", next)
	}
	later := time.Now().Add(h.steward.RequestSpacing() + time.Second)
	dec, err := st.Claim([]state.Key{keyOf("a@x.com")}, later)
	if err != nil || !dec[0].Permit {
		t.Fatalf("one spacing later the account cannot be asked: %+v %v", dec, err)
	}

	// SIGTERM mid-request: the next eligible account is b once its quiet
	// period is over; open it, and quiet a again.
	now = later
	quiet("a@x.com")
	writeJSON(t, filepath.Join(h.ownerRoot, "state.json"), clearNext(t, filepath.Join(h.ownerRoot, "state.json"), keyOf("b@x.com")))
	cmd = start()
	termAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a refresh asked to stop exited with %v", err)
		}
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatal("a refresh asked to stop was still running three seconds later")
	}
	t.Logf("stopped %v after SIGTERM", time.Since(termAt))
	if last := lastAttempt(t, filepath.Join(h.ownerRoot, "state.json"), keyOf("b@x.com")); last < termAt.UnixMilli() {
		t.Errorf("the abandoned request was not recorded: last attempt %d, signal at %d", last, termAt.UnixMilli())
	}
	if next := st.Load().NextEligible(keyOf("b@x.com"), time.Now()); !next.After(time.Now()) {
		t.Error("an abandoned request gave its account back before its spacing")
	}
}

// clearNext rewrites one account's quiet period to have passed, as time would.
func clearNext(t *testing.T, path string, k state.Key) string {
	t.Helper()
	var doc map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &doc) != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var accts map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["accounts"], &accts); err != nil {
		t.Fatal(err)
	}
	accts[k.ID()]["request"] = json.RawMessage(`{"last_attempt_ms":1,"next_eligible_ms":1}`)
	doc["accounts"], _ = json.Marshal(accts)
	out, _ := json.Marshal(doc)
	return string(out)
}

func lastAttempt(t *testing.T, path string, k state.Key) int64 {
	t.Helper()
	var doc struct {
		Accounts map[string]struct {
			Request struct {
				LastAttemptMS int64 `json:"last_attempt_ms"`
			} `json:"request"`
		} `json:"accounts"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &doc) != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return doc.Accounts[k.ID()].Request.LastAttemptMS
}

// A home whose `.ledger` cannot be used refuses every command, and check
// calls that broken, not untested: a doctor reading exit 2 as "could not
// test" must not pass a home that cannot launch.
func TestAnUnusableLedgerFailsCheck(t *testing.T) {
	bin := headroomBinary(t)
	h := newBinHomes(t, time.Now().Add(time.Hour))
	writeJSON(t, h.steward.LedgerFile(), filepath.Join(h.ownerRoot, "gone")+"\n")
	for args, want := range map[string]int{"check": 1, "launch --auto": 2} {
		cmd := exec.Command(bin, strings.Fields(args)...)
		cmd.Env = h.env()
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		if code != want || !strings.Contains(string(out), ".ledger") {
			t.Errorf("%s: exit %d, want %d:\n%s", args, code, want, out)
		}
	}
}

// `accounts ledger` joins a home to another's ledger, lists the homes there
// and, given the home's own root, leaves; `version` answers whatever the
// files say.
func TestJoiningLeavingAndVersion(t *testing.T) {
	bin := headroomBinary(t)
	h := newBinHomes(t, time.Now().Add(time.Hour))
	if err := os.Remove(h.steward.LedgerFile()); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = h.env()
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		return string(out), 0
	}

	out, code := run("accounts", "ledger", h.ownerRoot)
	if code != 0 || !strings.Contains(out, "shared") || !strings.Contains(out, h.steward.AccountsRoot+"  this home") {
		t.Fatalf("join: exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(h.steward.LedgerFile()); string(data) != h.ownerRoot+"\n" {
		t.Errorf(".ledger = %q", data)
	}
	if out, code = run("accounts", "ledger", h.steward.AccountsRoot); code != 0 || !strings.Contains(out, "this home's own") {
		t.Errorf("leave: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(h.steward.LedgerFile()); err == nil {
		t.Error("leaving left .ledger behind")
	}
	if _, code = run("accounts", "ledger", "--vendor", "codex"); code != 2 {
		t.Errorf("a Codex ledger was accepted: exit %d", code)
	}

	writeJSON(t, h.steward.LedgerFile(), "relative\n")
	out, code = run("version")
	if code != 0 || !regexp.MustCompile(`^headroom ([0-9a-f]{40}(\+dirty)? \S+|unknown)\n$`).MatchString(out) {
		t.Errorf("version under an unusable .ledger: exit %d %q", code, out)
	}
	if out, code = run("accounts", "ledger"); code != 2 || !strings.Contains(out, "delete it") {
		t.Errorf("an unusable .ledger: exit %d\n%s", code, out)
	}
}
