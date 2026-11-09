package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// loginFixture is a boardFixture whose accounts are judged by the real readers:
// credentials from each dir's .credentials.json (the stub Keychain finds
// nothing) and health from a stub `claude auth status` that says logged in.
func loginFixture(t *testing.T, usageURL string, extras ...string) autoFixture {
	t.Helper()
	f := newAutoFixture(t, extras...)
	f.cfg = withUsageURL(f.cfg, usageURL)
	f.st = state.Open(f.cfg)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"security": "#!/bin/sh\nexit 44\n",
		"claude":   "#!/bin/sh\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true}'; fi\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Judged by the launch path's own reader, not a stand-in for it.
	f.edges.Creds = LivePlace().Creds
	return f
}

func (f autoFixture) credential(name string, accessExpires, refreshExpires time.Time) {
	f.t.Helper()
	dir := filepath.Join(f.cfg.AccountsRoot, name)
	if name == "qiushi" {
		dir = f.cfg.PrimaryDir()
	}
	writeJSON(f.t, filepath.Join(dir, ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
			name, accessExpires.UnixMilli(), refreshExpires.UnixMilli()))
}

// One placement contract, one owner: what the board marks and what a launch
// takes are computed from the same evidence. The vendor's health probe may say
// "logged in" over a refresh token that has demonstrably expired; a launch
// goes by the credential, so the mark must too.
func TestTheBoardAndALaunchJudgeAccountsByTheSameEvidence(t *testing.T) {
	f := loginFixture(t, "http://127.0.0.1:1/usage", "a@x.com", "b@x.com")
	now := time.Now()
	f.credential("a@x.com", now.Add(-time.Hour), now.Add(-time.Hour)) // refresh token expired
	f.credential("b@x.com", now.Add(-time.Hour), now.Add(30*24*time.Hour))
	f.observe("qiushi", 60, 60, time.Minute)
	f.observe("a@x.com", 0, 5, time.Minute)
	f.observe("b@x.com", 20, 30, time.Minute)

	board := fetchBoards([]config.Scope{f.cfg}, f.boardDeps())[0]
	next := nextMarked(board.Accounts)
	got := f.launch()
	if got.code != 0 {
		t.Fatal(got.stderr)
	}
	if len(next) != 1 || next[0] != got.account(f.cfg) {
		t.Fatalf("the board marked %v and a launch with the same inputs took %s", next, got.account(f.cfg))
	}
	if got.account(f.cfg) != "b@x.com" {
		t.Fatalf("launched on %s, whose login has expired", got.account(f.cfg))
	}
}

// The launch path reads credentials through the reader the board uses: the
// Keychain first, then the account's own .credentials.json — the primary's
// included. An expired login found only in the primary's file must keep an
// automatic launch off the primary.
func TestPlacementReadsThePrimarysFileBackedCredential(t *testing.T) {
	f := loginFixture(t, "http://127.0.0.1:1/usage", "a@x.com")
	now := time.Now()
	f.credential("qiushi", now.Add(-time.Hour), now.Add(-time.Hour)) // refresh token expired
	f.credential("a@x.com", now.Add(time.Hour), now.Add(30*24*time.Hour))
	f.observe("qiushi", 0, 0, time.Minute)
	f.observe("a@x.com", 60, 60, time.Minute)

	got := f.launch()
	if got.code != 0 || got.account(f.cfg) != "a@x.com" {
		t.Fatalf("launched on %s:\n%s", got.account(f.cfg), got.stderr)
	}
	if note := dryNote(f.launch("--dry-run").stdout, "qiushi"); !strings.Contains(note, "excluded: login expired") {
		t.Errorf("the primary's file-backed credential was not read: %q", note)
	}
}

// The refresh an automatic launch leaves behind, through the real binary: the
// launch returns while the usage endpoint has not answered, and the answer is
// recorded afterwards by a process that outlived it.
func TestTheDetachedRefreshOutlivesTheLaunch(t *testing.T) {
	bin := headroomBinary(t)

	asked := make(chan string, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.Header.Get("Authorization")
		<-release
		w.Write([]byte(`{"limits":[{"kind":"session","group":"session","percent":7}]}`))
	}))
	t.Cleanup(srv.Close)
	// Closed before the server, so a stalled handler cannot hold Close up.
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})

	home := t.TempDir()
	stubs := t.TempDir()
	for name, body := range map[string]string{"claude": "#!/bin/sh\nexit 0\n", "security": "#!/bin/sh\nexit 44\n"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.AccountsRoot, "a@x.com")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u-a"}}`)
	far := time.Now().Add(time.Hour).UnixMilli()
	writeJSON(t, filepath.Join(dir, ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-a","expiresAt":%d,"refreshTokenExpiresAt":%d}}`, far, far))
	if err := os.Symlink(cfg.StoreDir(), filepath.Join(dir, "projects")); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "launch", "--", "-p", "hello")
	cmd.Env = append(os.Environ(),
		"HEADROOM_HOME="+home, "HEADROOM_PRIMARY_NAME=primary",
		"HEADROOM_USAGE_URL="+srv.URL, "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "a@x.com · auto") {
		t.Fatalf("launch output:\n%s", out)
	}
	t.Logf("launch returned in %v with the endpoint still silent", time.Since(start))

	// The launcher is gone. The request arrives from the process it left
	// behind, carrying the account's token…
	select {
	case auth := <-asked:
		if auth != "Bearer tok-a" {
			t.Errorf("the refresh asked with %q", auth)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no usage request arrived: the launch left no refresh behind it")
	}
	st := state.Open(cfg)
	key := state.Key{UUID: "u-a", Name: "a@x.com"}
	if _, ok := st.Load().Observation(key, time.Now()); ok {
		t.Fatal("an observation exists before the endpoint answered")
	}
	// …and its answer is recorded once the endpoint gives one.
	close(release)
	released = true
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := st.Load().Observation(key, time.Now()); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the endpoint answered and nothing recorded it: the refresh did not outlive the launch")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A launch that records where a session goes also clears the records of
// sessions whose transcripts are gone — whichever surface it came from. Left
// to the picker's `x` alone, an agent that names a new session id every run
// would grow the file for as long as nobody pressed `x`.
func TestALaunchThatRecordsASessionSweepsTheGoneOnes(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("a@x.com", 0, 5, time.Minute)
	const (
		gone  = "aaaaaaaa-1111-4111-8111-111111111111"
		kept  = "bbbbbbbb-2222-4222-8222-222222222222"
		young = "cccccccc-3333-4333-8333-333333333333"
	)
	old := time.Now().Add(-time.Hour)
	for id, at := range map[string]time.Time{gone: old, kept: old, young: time.Now().Add(-time.Minute)} {
		if err := rehome(f.st, id, "b@x.com", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.cfg.StoreDir(), "-proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(f.cfg.StoreDir(), "-proj", kept+".jsonl"), "{}\n")

	if got := f.launch("--", "-p", "--session-id", sessA); got.code != 0 {
		t.Fatalf("launch: %s", got.stderr)
	}
	snap := f.st.Load()
	if _, ok := snap.Owner(sessA); !ok {
		t.Fatal("the launch did not record its own session")
	}
	if _, ok := snap.Owner(gone); ok {
		t.Error("a re-home whose transcript is gone survived a launch that wrote one")
	}
	if _, ok := snap.Owner(kept); !ok {
		t.Error("a re-home whose transcript exists was swept")
	}
	if _, ok := snap.Owner(young); !ok {
		t.Error("a re-home too young to have a transcript yet was swept")
	}
}
