package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/signin"
)

// handoffWorld runs a handed-off sign-in in this process: a home short
// enough for the holder's socket, the login world's credentials and
// identities, holders run in goroutines with the login lock handed over as
// a duplicate descriptor (the process boundary's own sharing), and a vendor
// that prints Claude Code 2.1.311's screen and does with a code what the
// test says.
type handoffWorld struct {
	t       *testing.T
	cfg     config.Scope
	w       *loginWorld
	deps    handoffDeps
	holders sync.WaitGroup

	mu      sync.Mutex
	vendors []*fakeVendor
	// screen is what each new vendor prints; "" lines are left out. The
	// default is 2.1.311's, with the state the vendor is given.
	screen func(state string) []string
	// opened is the state the vendor hands its $BROWSER; default its own.
	opened func(state string) string
	// onCode is what a vendor does with a code: renew, by default, and exit.
	// hang leaves it running, as a vendor that stored the login and went
	// quiet would.
	onCode func(w *loginWorld, a string, code string) (exit error, hang bool)
}

func newHandoffWorld(t *testing.T) *handoffWorld {
	t.Helper()
	// t.TempDir's paths are too long for a socket (103 bytes).
	home, err := os.MkdirTemp("/tmp", "hr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, w := renewFixtureIn(t, cfg)
	h := &handoffWorld{t: t, cfg: cfg, w: w}
	h.screen = func(state string) []string {
		return []string{"Opening browser to sign in…", "If the browser didn't open, visit: " + signinURL(state), "Paste code here if prompted > "}
	}
	h.opened = func(state string) string { return state }
	h.onCode = func(w *loginWorld, a, _ string) (error, bool) {
		w.set(func() { w.raw[a] = blobEnding("new-"+a, w.now.Add(30*24*time.Hour)) })
		return nil, false
	}
	login := w.deps()
	login.signin = func(accounts.Account) string { return signin.None }
	h.deps = handoffDeps{
		login: login,
		vendor: func(acct accounts.Account, _ accounts.Set, _, opener string) (vendorRun, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			state := fmt.Sprintf("state-%d", len(h.vendors)+1)
			v := &fakeVendor{h: h, account: acct.Name, lines: make(chan string, 16), exited: make(chan error, 1)}
			for _, l := range h.screen(state) {
				v.lines <- l
			}
			if st := h.opened(state); st != "" {
				os.WriteFile(opener, []byte(st), 0o600)
			}
			h.vendors = append(h.vendors, v)
			return v, nil
		},
		ps:  sessions.PS,
		now: time.Now,
		bounds: handoffBounds{url: time.Second, opener: 200 * time.Millisecond, awaiting: 5 * time.Second,
			exchange: 2 * time.Second, stop: 100 * time.Millisecond, reply: 5 * time.Second, start: 5 * time.Second},
	}
	h.deps.spawn = func(name string, lock *os.File, paths signin.Paths) (signin.Record, error) {
		// The holder's own descriptor of the lock, as a child's would be: the
		// caller closing its copy leaves the lock held.
		fd, err := syscall.Dup(int(lock.Fd()))
		if err != nil {
			return signin.Record{}, err
		}
		held := os.NewFile(uintptr(fd), "lock")
		r, wr := io.Pipe()
		h.holders.Go(func() {
			defer held.Close()
			holdSignin(wr, io.Discard, h.cfg, name, h.deps)
			wr.Close()
		})
		var rec signin.Record
		line, _ := readLine(r)
		go io.Copy(io.Discard, r)
		if err := json.Unmarshal(line, &rec); err != nil {
			return rec, fmt.Errorf("the holder published %q", line)
		}
		return rec, nil
	}
	t.Cleanup(func() {
		for _, n := range []string{"primary", "a@x.com", "b@x.com", "c@x.com"} {
			loginCancel(Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}, h.cfg, n, h.deps)
		}
		h.holders.Wait()
	})
	return h
}

func readLine(r io.Reader) ([]byte, error) {
	var b []byte
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return b, nil
			}
			b = append(b, one[0])
		}
		if err != nil {
			return b, err
		}
	}
}

func signinURL(state string) string {
	return "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=" + state
}

// fakeVendor is `claude auth login` as a holder sees it.
type fakeVendor struct {
	h       *handoffWorld
	account string
	lines   chan string
	exited  chan error
	mu      sync.Mutex
	codes   []string
	once    sync.Once
	stopped bool
}

func (v *fakeVendor) Lines() <-chan string { return v.lines }
func (v *fakeVendor) Exited() <-chan error { return v.exited }
func (v *fakeVendor) Proc() signin.Proc    { return signin.Proc{} } // no pid: nothing ever kills it by number

func (v *fakeVendor) Send(code string) error {
	v.mu.Lock()
	v.codes = append(v.codes, code)
	v.mu.Unlock()
	exit, hang := v.h.onCode(v.h.w, v.account, code)
	if !hang {
		if exit != nil {
			v.lines <- "Paste code here if prompted > Login failed: That code wasn't accepted."
		}
		v.end(exit)
	}
	return nil
}

func (v *fakeVendor) Stop() {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	v.end(errors.New("signal: terminated"))
}

func (v *fakeVendor) end(err error) {
	v.once.Do(func() {
		close(v.lines)
		v.exited <- err
	})
}

// sent is every code the vendors received, in order.
func (h *handoffWorld) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var all []string
	for _, v := range h.vendors {
		v.mu.Lock()
		all = append(all, v.codes...)
		v.mu.Unlock()
	}
	return all
}

func (h *handoffWorld) vendorCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.vendors)
}

// signinDoc is a printed document, as a caller decodes it.
type signinDoc struct {
	Schema      int     `json:"schema"`
	Account     string  `json:"account"`
	State       string  `json:"state"`
	Reason      *string `json:"reason"`
	Problem     *string `json:"problem"`
	URL         string  `json:"url"`
	ExpiresAt   *string `json:"expires_at"`
	LoginEndsAt *string `json:"login_ends_at"`
	Reused      *bool   `json:"reused"`
}

func (d signinDoc) reason() string {
	if d.Reason == nil {
		return ""
	}
	return *d.Reason
}

// run calls one of the four surfaces and decodes what it printed.
func (h *handoffWorld) run(surface func(Streams, config.Scope, string, handoffDeps) error, name, stdin string) (signinDoc, int) {
	h.t.Helper()
	var out, errw bytes.Buffer
	r := finish(surface(Streams{In: strings.NewReader(stdin), Out: &out, Err: &errw}, h.cfg, name, h.deps), &out, &errw)
	var doc signinDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		h.t.Fatalf("not a document (exit %d): %q, stderr %q", r.code, out.String(), errw.String())
	}
	return doc, r.code
}

// The whole trip: start prints the URL the vendor printed — never the one it
// opened — and waits; finish with the code that URL's page shows renews, and
// status keeps the outcome. (login-handoff spec, test plan 5)
func TestHandoffStartThenFinishRenews(t *testing.T) {
	h := newHandoffWorld(t)
	doc, code := h.run(loginStart, "a@x.com", "")
	if code != 0 || doc.State != signin.Awaiting || doc.URL != signinURL("state-1") || doc.Reused == nil || *doc.Reused || doc.ExpiresAt == nil {
		t.Fatalf("start: exit %d, %+v", code, doc)
	}
	doc, code = h.run(loginFinish, "a@x.com", "the-code#state-1\n")
	if code != 0 || doc.State != signin.Renewed || doc.LoginEndsAt == nil || doc.URL != "" {
		t.Fatalf("finish: exit %d, %+v", code, doc)
	}
	if want := h.w.now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339); *doc.LoginEndsAt != want {
		t.Errorf("login_ends_at %s, want %s", *doc.LoginEndsAt, want)
	}
	if got := h.sent(); len(got) != 1 || got[0] != "the-code#state-1" {
		t.Errorf("the vendor received %q", got)
	}
	h.holders.Wait()
	if doc, code := h.run(loginStatus, "a@x.com", ""); code != 0 || doc.State != signin.Renewed {
		t.Errorf("status after: exit %d, %+v", code, doc)
	}
}

// A second start, or two at once, is one sign-in: the same URL, one vendor.
// A lost start answer never invalidates the URL already handed out.
// (test plan 6)
func TestHandoffStartIsOneSigninPerAccount(t *testing.T) {
	h := newHandoffWorld(t)
	var wg sync.WaitGroup
	docs := make([]signinDoc, 3)
	for i := range docs {
		wg.Go(func() { docs[i], _ = h.run(loginStart, "a@x.com", "") })
	}
	wg.Wait()
	reused := 0
	for _, d := range docs {
		if d.State != signin.Awaiting || d.URL != signinURL("state-1") {
			t.Errorf("start: %+v", d)
		}
		if d.Reused != nil && *d.Reused {
			reused++
		}
	}
	if reused != 2 || h.vendorCount() != 1 {
		t.Errorf("%d reused, %d vendors; want 2 and 1", reused, h.vendorCount())
	}
}

// A code headroom can tell is wrong is refused and never sent — the vendor
// would end the sign-in on it — and the sign-in still waits; a code is sent
// once, and a finish after the end is the receipt. (test plans 7, 8)
func TestHandoffSendsOneRightCode(t *testing.T) {
	h := newHandoffWorld(t)
	h.run(loginStart, "a@x.com", "")
	for _, c := range []struct{ line, reason string }{
		{"not a code\n", reasonMalformed},
		{"the-code#state-9\n", reasonOtherSignin},
	} {
		doc, code := h.run(loginFinish, "a@x.com", c.line)
		if code != 3 || doc.State != signin.Awaiting || doc.reason() != c.reason || doc.URL == "" {
			t.Errorf("%q: exit %d, %+v", c.line, code, doc)
		}
		if doc.Problem == nil || strings.Contains(*doc.Problem, strings.TrimSpace(c.line)) {
			t.Errorf("%q: the problem repeats the line or says nothing: %v", c.line, doc.Problem)
		}
	}
	if got := h.sent(); len(got) != 0 {
		t.Fatalf("the vendor received %q", got)
	}
	if doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 0 || doc.State != signin.Renewed {
		t.Fatalf("right code: exit %d, %+v", code, doc)
	}
	h.holders.Wait()
	if doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 0 || doc.State != signin.Renewed {
		t.Errorf("finish after the end: exit %d, %+v", code, doc)
	}
	if got := h.sent(); len(got) != 1 {
		t.Errorf("the vendor received %q, want one code", got)
	}
}

// The vendor refusing the code ends the sign-in: exit 1, its words kept.
func TestHandoffVendorRejection(t *testing.T) {
	h := newHandoffWorld(t)
	h.onCode = func(*loginWorld, string, string) (error, bool) { return errors.New("exit status 1"), false }
	h.run(loginStart, "a@x.com", "")
	doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n")
	if code != 1 || doc.State != signin.Failed || doc.reason() != vendorRejected || !strings.Contains(*doc.Problem, "That code wasn't accepted") {
		t.Errorf("exit %d, %+v", code, doc)
	}
}

// A vendor that stores the login and goes quiet is ended after the exchange
// bound and judged by its read-back; a finish whose answer is lost meanwhile
// says so, and status then has the outcome. (test plans 9, 12)
func TestHandoffQuietVendorAndLostReply(t *testing.T) {
	h := newHandoffWorld(t)
	h.deps.bounds.exchange = 500 * time.Millisecond
	h.onCode = func(w *loginWorld, a, _ string) (error, bool) {
		w.set(func() { w.raw[a] = blobEnding("new-"+a, w.now.Add(30*24*time.Hour)) })
		return nil, true
	}
	h.run(loginStart, "a@x.com", "")
	lost := h.deps
	lost.bounds.reply = 100 * time.Millisecond
	var out bytes.Buffer
	err := loginFinish(Streams{In: strings.NewReader("the-code#state-1\n"), Out: &out, Err: io.Discard}, h.cfg, "a@x.com", lost)
	var doc signinDoc
	json.Unmarshal(out.Bytes(), &doc)
	if !errors.Is(err, Status(1)) || doc.reason() != reasonNoReply || doc.State != signin.Exchanging {
		t.Fatalf("lost reply: %v, %+v", err, doc)
	}
	h.holders.Wait()
	if doc, _ := h.run(loginStatus, "a@x.com", ""); doc.State != signin.Renewed {
		t.Errorf("status: %+v", doc)
	}
	if got := h.sent(); len(got) != 1 {
		t.Errorf("the vendor received %q", got)
	}
}

// No code before the deadline ends the sign-in and its vendor; a code sent
// just before the deadline completes, since the exchange bound governs once
// a code is sent. (test plans 10, 11)
func TestHandoffDeadline(t *testing.T) {
	h := newHandoffWorld(t)
	h.deps.bounds.awaiting = 300 * time.Millisecond
	h.run(loginStart, "a@x.com", "")
	h.holders.Wait()
	if doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 1 || doc.State != signin.Failed || doc.reason() != reasonExpired {
		t.Errorf("after the deadline: exit %d, %+v", code, doc)
	}
	h.mu.Lock()
	stopped := h.vendors[0].stopped
	h.mu.Unlock()
	if !stopped || len(h.sent()) != 0 {
		t.Errorf("vendor stopped %v, codes %q", stopped, h.sent())
	}

	h2 := newHandoffWorld(t)
	h2.deps.bounds.awaiting = 400 * time.Millisecond
	h2.onCode = func(w *loginWorld, a, _ string) (error, bool) {
		time.Sleep(600 * time.Millisecond) // past the deadline
		w.set(func() { w.raw[a] = blobEnding("new-"+a, w.now.Add(30*24*time.Hour)) })
		return nil, false
	}
	h2.run(loginStart, "a@x.com", "")
	time.Sleep(200 * time.Millisecond)
	if doc, code := h2.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 0 || doc.State != signin.Renewed {
		t.Errorf("a code sent before the deadline: exit %d, %+v", code, doc)
	}
}

// A holder that died leaves its vendor running (measured, 2.1.311): finish
// reports the sign-in over before any code was sent, and the next start
// ends the vendor its record names — verified by pid and start — and begins
// afresh. (test plan 13)
func TestHandoffAfterTheHolderDied(t *testing.T) {
	h := newHandoffWorld(t)
	orphan := exec.Command("sleep", "60")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { orphan.Process.Kill() })
	exited := make(chan struct{})
	go func() { orphan.Wait(); close(exited) }()
	started, err := sessions.PS(orphan.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := accounts.Discover(h.cfg).Select("a@x.com")
	paths := signin.For(h.cfg.AccountsRoot, acct.Dir())
	paths.Ensure()
	signin.Write(paths.Record(), signin.Record{Account: "a@x.com", Email: "a@x.com", State: signin.Awaiting, URL: signinURL("old"),
		SigninState: "old", StartedMS: time.Now().UnixMilli(), Vendor: signin.Proc{PID: orphan.Process.Pid, Start: started}})

	if doc, code := h.run(loginFinish, "a@x.com", "the-code#old\n"); code != 1 || doc.State != signin.Failed || doc.reason() != reasonHolderGone {
		t.Errorf("finish: exit %d, %+v", code, doc)
	}
	select {
	case <-exited:
		t.Fatal("finish must not end the vendor; only a start or cancel, under the lock, does")
	default:
	}
	doc, code := h.run(loginStart, "a@x.com", "")
	if code != 0 || doc.State != signin.Awaiting || doc.URL != signinURL("state-1") {
		t.Errorf("start: exit %d, %+v", code, doc)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Error("the orphaned vendor was left running")
	}
}

// The batch and a handed-off sign-in never run one dir's login at once: the
// plan holds back an account whose sign-in is live, and start refuses an
// account the batch is renewing. (test plan 14)
func TestHandoffAndTheBatchExcludeEachOther(t *testing.T) {
	h := newHandoffWorld(t)
	h.run(loginStart, "a@x.com", "")
	batch := h.w.deps()
	batch.signin, batch.acquire = liveSignin(h.cfg), liveAcquire(h.cfg)
	var out bytes.Buffer
	if code := loginTo(&out, io.Discard, h.cfg, []string{"a@x.com"}, batch); code != 1 || len(h.w.logins) != 0 {
		t.Errorf("batch over a live sign-in: exit %d, logins %q\n%s", code, h.w.logins, out.String())
	}
	if line := planLine(out.String(), "a@x.com"); !strings.Contains(line, "a sign-in from headroom login start is in progress") {
		t.Errorf("plan line %q", line)
	}

	acct, _ := accounts.Discover(h.cfg).Select("b@x.com")
	paths := signin.For(h.cfg.AccountsRoot, acct.Dir())
	paths.Ensure()
	lock, ok, err := signin.Acquire(paths.Lock()) // the batch, renewing b
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	defer lock.Close()
	if doc, code := h.run(loginStart, "b@x.com", ""); code != 3 || doc.reason() != reasonBusy || h.vendorCount() != 1 {
		t.Errorf("start under the batch: exit %d, %+v, %d vendors", code, doc, h.vendorCount())
	}
}

// A request in another protocol version is refused before anything is
// acted on: an old holder may outlive an install. (test plan 15)
func TestHandoffRefusesAnotherProtocol(t *testing.T) {
	h := newHandoffWorld(t)
	h.run(loginStart, "a@x.com", "")
	acct, _ := accounts.Discover(h.cfg).Select("a@x.com")
	conn, err := net.Dial("unix", signin.For(h.cfg.AccountsRoot, acct.Dir()).Socket())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(conn, `{"v":2,"op":"finish","code":"the-code#state-1"}`)
	reply, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(reply), `"error":"protocol"`) || len(h.sent()) != 0 {
		t.Errorf("reply %s, codes %q", reply, h.sent())
	}
}

// The Keychain decides before any approval and again before the code goes
// out: an account whose login it holds and will not give is refused at
// start, and one it stops giving since is refused at finish, nothing sent.
func TestHandoffKeychain(t *testing.T) {
	h := newHandoffWorld(t)
	h.w.set(func() { h.w.keychain["a@x.com"], h.w.locked = true, true })
	if doc, code := h.run(loginStart, "a@x.com", ""); code != 3 || doc.reason() != heldKeychain || h.vendorCount() != 0 {
		t.Errorf("start: exit %d, %+v", code, doc)
	}
	h.w.set(func() { h.w.locked = false })
	if doc, code := h.run(loginStart, "a@x.com", ""); code != 0 || doc.State != signin.Awaiting {
		t.Fatalf("start, unlocked: exit %d, %+v", code, doc)
	}
	h.w.set(func() { h.w.locked = true })
	if doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 3 || doc.reason() != reasonKeychainShut || doc.State != signin.Awaiting || len(h.sent()) != 0 {
		t.Errorf("finish, relocked: exit %d, %+v, codes %q", code, doc, h.sent())
	}
}

// A screen with no URL headroom can read, or one whose printed URL is not
// the sign-in the vendor opened, starts nothing a code could reach.
func TestHandoffUnreadableScreen(t *testing.T) {
	h := newHandoffWorld(t)
	h.deps.bounds.url = 200 * time.Millisecond
	h.screen = func(state string) []string {
		return []string{"Opening browser to sign in…", "Visit the page to continue"}
	}
	if doc, code := h.run(loginStart, "a@x.com", ""); code != 1 || doc.State != signin.Failed || doc.reason() != reasonNoURL || !strings.Contains(*doc.Problem, "Visit the page to continue") {
		t.Errorf("no URL: exit %d, %+v", code, doc)
	}
	h.holders.Wait()
	h.screen = func(state string) []string { return []string{"visit: " + signinURL(state)} }
	h.opened = func(string) string { return "another" }
	if doc, code := h.run(loginStart, "a@x.com", ""); code != 1 || doc.reason() != reasonScreenChanged {
		t.Errorf("another sign-in opened: exit %d, %+v", code, doc)
	}
}

// Cancel ends a waiting sign-in and its vendor; nothing to cancel is fine.
func TestHandoffCancel(t *testing.T) {
	h := newHandoffWorld(t)
	if doc, code := h.run(loginCancel, "a@x.com", ""); code != 0 || doc.State != signin.None {
		t.Errorf("nothing to cancel: exit %d, %+v", code, doc)
	}
	h.run(loginStart, "a@x.com", "")
	if doc, code := h.run(loginCancel, "a@x.com", ""); code != 0 || doc.State != signin.Failed || doc.reason() != reasonCancelled {
		t.Errorf("cancel: exit %d, %+v", code, doc)
	}
	h.holders.Wait()
	if doc, code := h.run(loginFinish, "a@x.com", "the-code#state-1\n"); code != 1 || doc.reason() != reasonCancelled || len(h.sent()) != 0 {
		t.Errorf("finish after cancel: exit %d, %+v", code, doc)
	}
	if doc, code := h.run(loginStart, "a@x.com", ""); code != 0 || doc.URL != signinURL("state-2") {
		t.Errorf("start after cancel: exit %d, %+v", code, doc)
	}
}

// The plan as a document: every account, unknown ends null, the sign-in's
// state beside each. (test plan 4)
func TestLoginPlanDocument(t *testing.T) {
	cfg, w := renewFixture(t)
	var out bytes.Buffer
	r := call(func(std Streams) error {
		std.Out = &out
		return renewLogins(std, cfg, LoginOptions{Within: DefaultLoginWithin, DryRun: true, JSON: true}, w.deps())
	})
	var doc struct {
		Schema     int `json:"schema"`
		WithinDays int `json:"within_days"`
		Accounts   []struct {
			Account     string  `json:"account"`
			Email       *string `json:"email"`
			Renew       bool    `json:"renew"`
			Held        *string `json:"held"`
			LoginEndsAt *string `json:"login_ends_at"`
			Signin      string  `json:"signin"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || r.code != 0 {
		t.Fatalf("exit %d, %v: %s", r.code, err, out.String())
	}
	if doc.Schema != 1 || doc.WithinDays != 7 || len(doc.Accounts) != 4 || len(w.logins) != 0 {
		t.Fatalf("%+v, logins %q", doc, w.logins)
	}
	p, a, b := doc.Accounts[0], doc.Accounts[1], doc.Accounts[2]
	if p.Account != "primary" || p.Email != nil || p.LoginEndsAt != nil || !p.Renew {
		t.Errorf("primary never logged in: %+v", p)
	}
	if a.Account != "a@x.com" || !a.Renew || a.LoginEndsAt == nil || *a.LoginEndsAt != w.now.Add(3*24*time.Hour).UTC().Format(time.RFC3339) || a.Held != nil || a.Signin != signin.None {
		t.Errorf("a: %+v", a)
	}
	if b.Renew {
		t.Errorf("b ends in 20d and was chosen: %+v", b)
	}
}
