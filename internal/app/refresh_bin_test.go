package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// binHome lays out, for the real binary, a home with a primary and two extra
// accounts holding file-backed logins: what a detached refresh runs against.
// Nothing points HEADROOM_* anywhere — the binary finds the home from HOME
// alone, as it does under launchd.
type binHome struct {
	home  string
	scope config.Scope // as the binary resolves it
	stubs string
}

func newBinHome(t *testing.T, access time.Time) binHome {
	t.Helper()
	h := binHome{home: filepath.Join(t.TempDir(), "home")}
	h.scope = config.ForHome(h.home).Claude
	if err := os.MkdirAll(h.scope.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	far := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	login := func(meta, dir, email string) {
		writeJSON(t, meta, fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, email, email))
		writeJSON(t, filepath.Join(dir, ".credentials.json"),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, email, access.UnixMilli(), far))
	}
	login(h.scope.PrimaryMeta(), h.scope.PrimaryDir(), "yan@x.com")
	for _, email := range []string{"a@x.com", "b@x.com"} {
		dir := filepath.Join(h.scope.AccountsRoot, email)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		login(filepath.Join(dir, ".claude.json"), dir, email)
		if err := os.Symlink(h.scope.StoreDir(), filepath.Join(dir, "projects")); err != nil {
			t.Fatal(err)
		}
	}
	// security finds no Keychain.
	h.stubs = t.TempDir()
	if err := os.WriteFile(filepath.Join(h.stubs, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// env is a launchd-sized environment: HOME, a PATH whose first entry holds
// the stubs, and nothing of the owner's shell.
func (h binHome) env(extra ...string) []string {
	return append([]string{
		"HOME=" + h.home, "USER=" + os.Getenv("USER"),
		"PATH=" + h.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
	}, extra...)
}

// What a launch leaves running is a refresh round, and the steward's
// supervisor ends it with its job: SIGTERM, then SIGKILL fifteen seconds
// later. Killed outright mid-request, it leaves the shared store readable,
// unlocked, and holding a claim that expires at its spacing. Asked to stop, it
// stops within a second and records the request it abandoned.
func TestARefreshKilledMidRequestLeavesTheStoreUsable(t *testing.T) {
	bin := headroomBinary(t)
	h := newBinHome(t, time.Now().Add(time.Hour))
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

	st := state.Open(h.scope)
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
	if !next.After(time.Now()) || next.After(time.Now().Add(h.scope.RequestSpacing())) {
		t.Errorf("the killed request's claim stands until %v — want a deadline within one spacing", next)
	}
	later := time.Now().Add(h.scope.RequestSpacing() + time.Second)
	dec, err := st.Claim([]state.Key{keyOf("a@x.com")}, later)
	if err != nil || !dec[0].Permit {
		t.Fatalf("one spacing later the account cannot be asked: %+v %v", dec, err)
	}

	// SIGTERM mid-request: the next eligible account is b once its quiet
	// period is over; open it, and quiet a again.
	now = later
	quiet("a@x.com")
	writeJSON(t, filepath.Join(h.scope.AccountsRoot, "state.json"), clearNext(t, filepath.Join(h.scope.AccountsRoot, "state.json"), keyOf("b@x.com")))
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
	if last := lastAttempt(t, filepath.Join(h.scope.AccountsRoot, "state.json"), keyOf("b@x.com")); last < termAt.UnixMilli() {
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

// The refresh stops when told to whatever it is doing: reading a credential
// through `security` included, which can stall where the vendor's own
// keychain does.
func TestARefreshStopsDuringCredentialReads(t *testing.T) {
	bin := headroomBinary(t)
	h := newBinHome(t, time.Now().Add(time.Hour))
	reading := filepath.Join(t.TempDir(), "reading")
	if err := os.WriteFile(filepath.Join(h.stubs, "security"), []byte("#!/bin/sh\ntouch "+reading+"\nsleep 30\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "refresh")
	cmd.Env = h.env("HEADROOM_USAGE_URL=http://127.0.0.1:1/usage")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(reading); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the refresh never read a credential")
		}
		time.Sleep(20 * time.Millisecond)
	}
	termAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Logf("stopped %v after SIGTERM", time.Since(termAt))
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("a refresh asked to stop while reading a credential was still running three seconds later")
	}
	if snap := state.Open(h.scope).Load(); len(snap.Audit()) != 0 {
		t.Errorf("a refresh stopped before asking anything claimed: %+v", snap.Audit())
	}
}
