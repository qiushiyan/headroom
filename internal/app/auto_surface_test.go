package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tui"
)

// boardFixture is an autoFixture a reporting surface can run over: no Keychain
// (a stub that finds nothing), and a usage URL nothing listens on, so no
// request can leave whatever the round decides.
func boardFixture(t *testing.T, extras ...string) autoFixture {
	t.Helper()
	f := newAutoFixture(t, extras...)
	f.cfg = withUsageURL(f.cfg, "http://127.0.0.1:1/usage")
	f.st = state.Open(f.cfg)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func marks(list []*accountData) (current, next []string) {
	for _, d := range list {
		if d.View.Current {
			current = append(current, d.Acct.Name)
		}
		if d.View.Next {
			next = append(next, d.Acct.Name)
		}
	}
	return
}

// With inputs frozen and no placement in between, the row a board marks is the
// account a launch then takes. Under a pin nothing is marked next, and under
// auto nothing is marked current.
func TestTheBoardMarksWhatALaunchWouldTake(t *testing.T) {
	f := boardFixture(t, "a@x.com", "b@x.com")
	f.observe("qiushi", 30, 20, time.Minute)
	f.observe("a@x.com", 0, 40, time.Minute)
	f.observe("b@x.com", 0, 10, time.Minute)

	board := fetchBoards([]config.Scope{f.cfg})[0]
	if current, next := marks(board.list); len(current) != 1 || current[0] != "qiushi" || len(next) != 0 || board.mode != "pinned" {
		t.Fatalf("pinned: current %v next %v mode %q", current, next, board.mode)
	}

	f.setCurrent("auto\n")
	board = fetchBoards([]config.Scope{f.cfg})[0]
	current, next := marks(board.list)
	if len(current) != 0 || len(next) != 1 || board.mode != "auto" {
		t.Fatalf("auto: current %v next %v mode %q", current, next, board.mode)
	}
	if got := f.launch(); got.account(f.cfg) != next[0] {
		t.Fatalf("the board marked %s and the launch took %s:\n%s", next[0], got.account(f.cfg), got.stderr)
	}

	// Both layouts and the one-shot print carry the mark and say the mode.
	p := render.NewPalette(false)
	for layout, mark := range map[render.Layout]string{render.LayoutBlocks: "← next", render.LayoutCompact: "→"} {
		var out bytes.Buffer
		writeBoards(&out, p, []vendorBoard{board}, time.Now().Unix(), layout, 0)
		text := out.String()
		if !strings.Contains(text, "bare launches are automatic") || strings.Contains(text, "← current") || strings.Contains(text, "●") {
			t.Errorf("layout %v does not say the mode:\n%s", layout, text)
		}
		marked := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, next[0]) && strings.Contains(line, mark) {
				marked++
			}
		}
		if marked != 1 {
			t.Errorf("layout %v: %d rows carry %q for %s:\n%s", layout, marked, mark, next[0], text)
		}
	}
}

func TestSchema6SaysTheMode(t *testing.T) {
	f := boardFixture(t, "a@x.com")
	doc := func() doc5 {
		t.Helper()
		data, err := jsonDocument(fetchBoards([]config.Scope{f.cfg}), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return decode5(t, data)
	}
	limits := func() doc5 {
		t.Helper()
		var buf bytes.Buffer
		if code := runLimitsTo(&buf, []config.Scope{f.cfg}, nil); code != 0 {
			t.Fatalf("limits: exit %d", code)
		}
		return decode5(t, buf.Bytes())
	}
	check := func(name, mode, current string) {
		t.Helper()
		for surface, d := range map[string]doc5{"--json": doc(), "limits": limits()} {
			marked := 0
			for _, a := range d.Accounts {
				if a.Current {
					marked++
				}
			}
			wantMarked := 0
			if current != "" {
				wantMarked = 1
			}
			if d.Schema != 6 || d.Mode["claude"] != mode || d.Current["claude"] != current || marked != wantMarked {
				t.Errorf("%s under %s: schema %d mode %q current %q, %d marked current", surface, name, d.Schema, d.Mode["claude"], d.Current["claude"], marked)
			}
		}
	}
	check("the fresh-start default", "pinned", "qiushi")
	f.setCurrent("a@x.com\n")
	check("a pin", "pinned", "a@x.com")
	f.setCurrent("auto\n")
	check("auto", "auto", "")
	f.setCurrent("gone@x.com\n")
	check("an unresolvable .current", "", "")
}

func TestTheAKeyAsksForAuto(t *testing.T) {
	pg, _, _ := injectedPage(t, config.Claude, "c0@x.com")
	ui := &picker{pages: []*page{pg}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	ui.show(ctx, 0)
	keys := make(chan tui.Key, 1)
	keys <- tui.Key{Kind: tui.KeyRune, Rune: 'a'}
	if got := ui.step(ctx, nil, keys); got != stepAuto {
		t.Fatalf("a → %v, want stepAuto", got)
	}
	if !strings.Contains(ui.status(time.Now()), "a auto") {
		t.Error("the footer does not offer the key")
	}

	// What the key commits: the visible vendor's `.current`, and nothing else.
	f := boardFixture(t, "a@x.com")
	set := accounts.Discover(f.cfg)
	if err := set.SetAuto(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.cfg.CurrentFile()); string(data) != "auto\n" {
		t.Errorf(".current = %q", data)
	}
	// Enter pins again.
	a, _ := set.Select("a@x.com")
	if err := set.SetCurrent(a); err != nil {
		t.Fatal(err)
	}
	if got := accounts.Discover(f.cfg).Mode(); got != "pinned" {
		t.Errorf("mode after enter = %q", got)
	}
	// The word cannot be written as a pin.
	if err := set.SetCurrent(accounts.Account{Scope: f.cfg, Name: accounts.AutoWord}); err == nil {
		t.Error("an account named with the reserved word was pinned")
	}
}

func TestRenderMarksNextOnlyWhereThereIsNoCurrent(t *testing.T) {
	p := render.NewPalette(false)
	next := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Next: true}
	if line := p.HeaderLine(next); !strings.Contains(line, "← next") || strings.Contains(line, "← current") {
		t.Errorf("header = %q", line)
	}
	both := accountstate.Facts{Label: "a@x.com", Launcher: "x-a", Current: true, Next: true}
	if line := p.HeaderLine(both); strings.Contains(line, "← next") {
		t.Errorf("a current account was also marked next: %q", line)
	}
	b := p.Board([]accountstate.Facts{next, {Label: "b@x.com", Launcher: "x-b"}}, time.Now().Unix(), render.LayoutCompact, 0)
	if !strings.Contains(b.Groups[0][0], "→") || strings.Contains(b.Groups[1][0], "→") {
		t.Errorf("compact rows = %q / %q", b.Groups[0][0], b.Groups[1][0])
	}
}

// The picker under auto: where it used to fall back to the current account it
// asks the rule, and the override moves the session somewhere other than where
// it is.
func TestThePickerPlacesUnderAuto(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 50, 50, time.Minute)
	f.observe("a@x.com", 0, 5, time.Minute)
	f.observe("b@x.com", 0, 30, time.Minute)
	proj := filepath.Join(f.cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	set := accounts.Discover(f.cfg)
	newUI := func(owner string, state sessions.OwnerState) (*resumeUI, *sessions.Session) {
		ui := &resumeUI{sessionActions: sessionActions{beforeLaunch: func() {},
			cfg: f.cfg, st: f.st, set: set, auto: set.Mode() == "auto",
		}}
		s := &sessions.Session{ID: sessA, CWD: proj, DirOK: true, Owner: owner, OwnerState: state}
		ui.listing.Sessions = []*sessions.Session{s}
		ui.rows = ui.listing.Sessions
		return ui, s
	}
	account := func(env []string) string {
		if dir, ok := envValue(env, "CLAUDE_CONFIG_DIR"); ok {
			return filepath.Base(dir)
		}
		return "qiushi"
	}

	// No evidence: placed like a new session, on the least-loaded account.
	ui, _ := newUI("", sessions.OwnerNone)
	_, _, env, called := capturedSessionsExec(t)
	if done, code := ui.commitResume(false); !done || code != 0 || !*called {
		t.Fatalf("enter with no owner: (%v, %d) called %v — %s", done, code, *called, ui.message)
	}
	if got := account(*env); got != "a@x.com" {
		t.Fatalf("placed on %s, want a@x.com", got)
	}
	if _, ok := f.st.Load().Owner(sessA); ok {
		t.Error("an ordinary resume wrote a re-home")
	}

	// An owner that exists is followed, auto or not.
	ui, _ = newUI("b@x.com", sessions.OwnerHistory)
	_, _, env, _ = capturedSessionsExec(t)
	if done, _ := ui.commitResume(false); !done || account(*env) != "b@x.com" {
		t.Fatalf("enter on an owned session went to %s", account(*env))
	}
	// Every session headroom starts is a line in the log and load on its
	// account, a resume on its owner included — once each.
	if recs := f.log(); len(recs) != 2 || recs[1].Mode != "picker" || recs[1].Reason != "picker" || recs[1].Chosen != "b@x.com" {
		t.Fatalf("log after two resumes = %+v", recs)
	}
	if n := len(f.st.Load().Placements().Recent); n != 2 {
		t.Fatalf("%d placements recorded after two resumes", n)
	}

	// The override: the least-loaded of the *other* accounts, re-homed, logged.
	ui, s := newUI("a@x.com", sessions.OwnerHistory)
	_, _, env, _ = capturedSessionsExec(t)
	if done, code := ui.commitResume(true); !done || code != 0 {
		t.Fatalf("x: (%v, %d) — %s", done, code, ui.message)
	}
	moved := account(*env)
	if moved == "a@x.com" {
		t.Fatal("x left the session where it was")
	}
	if rec, _ := f.st.Load().Owner(s.ID); rec.Account != moved {
		t.Errorf("re-home = %+v, want %s", rec, moved)
	}
	recs := f.log()
	last := recs[len(recs)-1]
	if len(recs) != 3 || last.Mode != "picker" || last.Session != sessA || last.Chosen != moved {
		t.Errorf("log = %+v", last)
	}

	// With nowhere else to go, x says so and commits nothing.
	solo := newAutoFixture(t)
	solo.setCurrent("auto\n")
	soloSet := accounts.Discover(solo.cfg)
	ui = &resumeUI{sessionActions: sessionActions{beforeLaunch: func() {}, cfg: solo.cfg, st: solo.st, set: soloSet, auto: true}}
	sess := &sessions.Session{ID: sessB, CWD: proj, DirOK: true, Owner: "qiushi", OwnerState: sessions.OwnerHistory}
	ui.listing.Sessions = []*sessions.Session{sess}
	ui.rows = ui.listing.Sessions
	_, _, _, called = capturedSessionsExec(t)
	if done, _ := ui.commitResume(true); done || *called || !strings.Contains(ui.message, "already there") {
		t.Fatalf("x with one account: done %v called %v message %q", done, *called, ui.message)
	}
}

// Codex follows its own mode: its own `.current`, its own rows, its own record
// — and no busy sessions, no session routing, and nothing of Claude Code's.
func TestCodexFollowsItsOwnMode(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1/usage")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	prevRefresh := startRefresh
	startRefresh = func(config.Scope) error { return nil }
	t.Cleanup(func() { startRefresh = prevRefresh })

	f.extra(t, login(1))
	f.extra(t, login(2))
	f.extra(t, login(3))
	st := state.Open(f.scope)
	store := func(n, percent int, blocked bool) {
		t.Helper()
		l := login(n)
		body := codexUsage(l.AccountID, l.UserID, percent)
		if blocked {
			body = strings.Replace(body, `"limit_reached":false`, `"limit_reached":true`, 1)
		}
		k := state.Key{UUID: "uuid:" + l.AccountID + "/" + l.UserID, Name: l.Email}
		for _, a := range accounts.Discover(f.scope).Accounts {
			if a.Name == l.Email {
				k = state.Key{UUID: a.AccountID, Name: a.Name}
			}
		}
		at := time.Now().Add(-time.Minute)
		dec, err := st.Claim([]state.Key{k}, at)
		if err != nil || !dec[0].Permit {
			t.Fatalf("claim: %+v %v", dec, err)
		}
		if _, err := st.Complete(k, dec[0].Generation, state.OutcomeStored, []byte(body), at); err != nil {
			t.Fatal(err)
		}
	}
	store(1, 55, false)
	store(2, 12, false)
	store(3, 0, true) // the emptiest, and the vendor refuses work on it

	if err := os.MkdirAll(f.scope.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.scope.CurrentFile(), []byte("auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeCurrent := f.cfg.Claude.CurrentFile()

	launch := func(args ...string) (string, string) {
		t.Helper()
		_, _, env, called := recordExec(t)
		var code int
		stderr := captureStderr(t, func() { code = runLaunch(f.scope, args) })
		if code != 0 || !*called {
			t.Fatalf("exit %d called %v: %s", code, *called, stderr)
		}
		home, _ := envValue(*env, "CODEX_HOME")
		return filepath.Base(home), stderr
	}
	first, stderr := launch("--", "resume", "--all")
	if first != "u2@x.com" {
		t.Fatalf("launched on %q, want u2@x.com (u3 is blocked, u1 is fuller):\n%s", first, stderr)
	}
	if !strings.Contains(stderr, "· auto ·") || strings.Contains(stderr, "session") {
		t.Errorf("a Codex `resume` is placed like a new session and says nothing of one:\n%s", stderr)
	}
	// Its own placement is one step; ten points of the window are another.
	if second, _ := launch(); second == "u3@x.com" {
		t.Fatal("a blocked account was chosen")
	}
	recs, _, _ := readLog(t, f.scope)
	for _, c := range recs[0].Candidates {
		if c.Name == "u3@x.com" && c.Excluded != "blocked by the vendor" {
			t.Errorf("u3 excluded %q", c.Excluded)
		}
		if c.Busy != 0 || len(c.Statuses) != 0 {
			t.Errorf("%s carries busy sessions Codex cannot report: %+v", c.Name, c)
		}
	}
	if recs[0].Vendor != "codex" || recs[0].Reason != placement.ReasonLeastLoad {
		t.Errorf("log = %+v", recs[0])
	}
	if _, err := os.Stat(claudeCurrent); err == nil {
		t.Error("a Codex launch wrote Claude Code's .current")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Claude.AccountsRoot, "state.json")); err == nil {
		t.Error("a Codex launch wrote Claude Code's state.json")
	}
}

func TestLaunchLineWording(t *testing.T) {
	now := time.Unix(1_790_848_000, 0)
	obs := func(name string, session, weekly int, age time.Duration) placement.Candidate {
		return placement.Candidate{Name: name, Key: name, ObservedAt: now.Add(-age).Unix(), Limits: []placement.Limit{
			{Kind: "session", Label: "5h session", Percent: session, ResetAt: now.Add(time.Hour).Unix(), Session: true},
			{Kind: "weekly_all", Label: "All models (7d)", Percent: weekly, ResetAt: now.Add(29 * time.Hour).Unix()},
		}}
	}
	choose := func(intent placement.Intent, cands ...placement.Candidate) placement.Decision {
		return placement.Choose(cands, placement.Ledger{}, intent, now)
	}
	cases := []struct {
		name  string
		d     placement.Decision
		mode  string
		ref   sessionRef
		owner string
		want  []string
		not   []string
	}{
		{"least load", choose(placement.Intent{}, obs("a", 12, 3, time.Minute), obs("b", 40, 3, time.Minute)), "auto", sessionRef{}, "",
			[]string{"a · auto · 5h 12% · week 3% · load 1 (next: b)"}, []string{"old"}},
		{"stale", choose(placement.Intent{}, obs("a", 0, 18, 40*time.Hour)), "auto", sessionRef{}, "",
			[]string{"a · auto · 5h 0% · week 18% · figures 1d old · load 0"}, []string{"next"}},
		{"never observed", choose(placement.Intent{}, placement.Candidate{Name: "a", Key: "a"}), "auto", sessionRef{}, "",
			[]string{"a · auto · never observed · load 0"}, nil},
		{"near a limit", choose(placement.Intent{}, obs("a", 10, 84, time.Minute)), "auto", sessionRef{}, "",
			[]string{"a · auto · every account is near a limit · All models (7d) 84%, resets in 1.2d"}, []string{"load"}},
		{"owner", choose(placement.Intent{Owner: "a"}, obs("a", 30, 30, time.Minute), obs("b", 0, 0, time.Minute)), "auto", sessionRef{Route: sessA}, "a",
			[]string{"a · auto · this session's account"}, nil},
		{"moved", choose(placement.Intent{Owner: "a"}, obs("a", 30, 95, time.Minute), obs("b", 0, 0, time.Minute)), "auto", sessionRef{Route: sessA}, "a",
			[]string{"b · auto · session moved from a"}, nil},
		{"unidentified", choose(placement.Intent{}, obs("a", 0, 0, time.Minute)), "auto", sessionRef{Unidentified: true}, "",
			[]string{"session not identified"}, nil},
		{"pinned", choose(placement.Intent{Kind: placement.Forced, Account: "a", Reason: "pinned"}, obs("a", 0, 0, time.Minute)), "pinned", sessionRef{}, "",
			[]string{"a · pinned (a on the board turns auto on)"}, []string{"auto ·"}},
	}
	for _, c := range cases {
		line := launchLine(c.d, c.mode, c.ref, c.owner, now)
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q lacks %q", c.name, line, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(line, n) {
				t.Errorf("%s: %q must not say %q", c.name, line, n)
			}
		}
	}
	// A session that named an id nobody owned.
	d := choose(placement.Intent{}, obs("a", 0, 0, time.Minute))
	d.Reason = placement.ReasonMoved
	if line := launchLine(d, "auto", sessionRef{Route: sessA}, "", now); !strings.Contains(line, "session had no known account") {
		t.Errorf("unowned: %q", line)
	}
	_ = fmt.Sprint
}
