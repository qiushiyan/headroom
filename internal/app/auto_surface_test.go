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
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
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

// nextMarked is the accounts a board marks as the one a launch would take.
func nextMarked(list []accountstate.Account) (next []string) {
	for _, d := range list {
		if d.View.Next {
			next = append(next, d.Acct.Name)
		}
	}
	return next
}

// With inputs frozen and no placement in between, the row a board marks is the
// account a launch then takes.
func TestTheBoardMarksWhatALaunchWouldTake(t *testing.T) {
	f := boardFixture(t, "a@x.com", "b@x.com")
	f.observe("qiushi", 30, 20, time.Minute)
	f.observe("a@x.com", 0, 40, time.Minute)
	f.observe("b@x.com", 0, 10, time.Minute)

	boards := fetchBoards([]config.Scope{f.cfg}, f.boardDeps())
	next := nextMarked(boards[0].Accounts)
	if len(next) != 1 {
		t.Fatalf("marked next: %v", next)
	}
	if got := f.launch(); got.account(f.cfg) != next[0] {
		t.Fatalf("the board marked %s and the launch took %s:\n%s", next[0], got.account(f.cfg), got.stderr)
	}

	// Both layouts and the one-shot print carry the mark on one row.
	p := render.NewPalette(false)
	for layout, mark := range map[render.Layout]string{render.LayoutBlocks: "← next", render.LayoutCompact: "→"} {
		var out bytes.Buffer
		writeBoards(&out, p, boards, time.Now().Unix(), layout, 0)
		text := out.String()
		marked := 0
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, next[0]) && strings.Contains(line, mark) {
				marked++
			}
		}
		if marked != 1 {
			t.Errorf("layout %v: %d rows carry %q for %s:\n%s", layout, marked, mark, next[0], text)
		}
	}
}

// A board left open marks what a launch would take at the moment it draws, not
// at its last round: with nothing re-read, a weekly window ending soon catches
// up with room that lasts longer, and the mark moves with it.
func TestAnOpenBoardMarksAtEachFrame(t *testing.T) {
	f := boardFixture(t, "a@x.com", "b@x.com")
	f.observe("qiushi", 30, 0, time.Minute)
	f.observeWeek("a@x.com", 0, 70, time.Hour, time.Minute)
	f.observeWeek("b@x.com", 0, 0, 7*time.Hour, time.Minute)

	pg := newPage(f.cfg, f.boardDeps())
	var out strings.Builder
	ui := &picker{pages: []*page{pg}, p: render.NewPalette(false), lastKey: time.Now(),
		fp: &framePrinter{out: &out, size: func() (int, int, error) { return 120, 60, nil }}}
	ui.show(context.Background(), 0)
	next := func() string {
		n := nextMarked(pg.accounts())
		if len(n) != 1 {
			t.Fatalf("%d rows marked next", len(n))
		}
		return n[0]
	}
	if got := next(); got != "b@x.com" {
		t.Fatalf("now: marked %s; 80 points over seven hours beat 10 over one", got)
	}
	pg.markAt(time.Now().Add(10 * time.Minute))
	if got := next(); got != "a@x.com" {
		t.Fatalf("ten minutes on: marked %s; a's 10 points now end within the hour", got)
	}
	ui.draw()
	if got := next(); got != "b@x.com" {
		t.Errorf("the frame drawn now marked %s, not what a launch now takes", got)
	}
}

// The board records no routing: enter and a, the keys that once pinned the
// selected row and turned auto on, are keys it does not have, and its footer
// does not offer them. A stray enter on a board opened to read it pinned
// whatever row the cursor was on.
func TestTheBoardHasNoRoutingKeys(t *testing.T) {
	pg, _, _ := injectedPage(t, config.Claude, "c0@x.com", "c1@x.com")
	ui := &picker{pages: []*page{pg}, p: render.NewPalette(false), lastKey: time.Now()}
	ctx := context.Background()
	ui.show(ctx, 0)
	keys := make(chan tui.Key, 1)
	for _, k := range []tui.Key{{Kind: tui.KeyEnter}, {Kind: tui.KeyRune, Rune: 'a'}} {
		keys <- k
		if ui.step(ctx, nil, keys) {
			t.Errorf("key %+v quit the board", k)
		}
	}
	for _, scrolls := range []bool{false, true} {
		if footer := ui.status(time.Now(), scrolls); strings.Contains(footer, "enter") || strings.Contains(footer, "a auto") {
			t.Errorf("the footer offers a routing key: %q", footer)
		}
	}
}

// The picker: a session with no owner to follow is placed by the rule, and the
// override moves the session somewhere other than where it is.
func TestThePickerPlaces(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("qiushi", 50, 50, time.Minute)
	f.observe("a@x.com", 0, 5, time.Minute)
	f.observe("b@x.com", 0, 30, time.Minute)
	proj := filepath.Join(f.cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	set := accounts.Discover(f.cfg)
	newUI := func(owner string, state sessions.OwnerState) (*resumeUI, *sessions.Session) {
		ui, _, _ := f.picker(set)
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
	keepWorkingDir(t)
	rec := rearm(ui)
	if done, code := ui.commitResume(false); !done || code != 0 || !rec.called {
		t.Fatalf("enter with no owner: (%v, %d) called %v — %s", done, code, rec.called, ui.message)
	}
	if got := account(rec.env); got != "a@x.com" {
		t.Fatalf("placed on %s, want a@x.com", got)
	}
	if _, ok := f.st.Load().Owner(sessA); ok {
		t.Error("an ordinary resume wrote a re-home")
	}

	// An owner that exists is followed.
	ui, _ = newUI("b@x.com", sessions.OwnerHistory)
	keepWorkingDir(t)
	rec = rearm(ui)
	if done, _ := ui.commitResume(false); !done || account(rec.env) != "b@x.com" {
		t.Fatalf("enter on an owned session went to %s", account(rec.env))
	}
	// Every session headroom starts is a line in the log and load on its
	// account, a resume on its owner included — once each.
	if recs := f.log(); len(recs) != 2 || recs[1].Mode != "picker" || recs[1].Reason != placement.ReasonOwner || recs[1].Chosen != "b@x.com" {
		t.Fatalf("log after two resumes = %+v", recs)
	}
	if n := len(f.st.Load().Placements().Recent); n != 2 {
		t.Fatalf("%d placements recorded after two resumes", n)
	}

	// The override: the least-loaded of the *other* accounts, re-homed, logged.
	ui, s := newUI("a@x.com", sessions.OwnerHistory)
	keepWorkingDir(t)
	rec = rearm(ui)
	if done, code := ui.commitResume(true); !done || code != 0 {
		t.Fatalf("x: (%v, %d) — %s", done, code, ui.message)
	}
	moved := account(rec.env)
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
	soloSet := accounts.Discover(solo.cfg)
	ui, rec, _ = solo.picker(soloSet)
	sess := &sessions.Session{ID: sessB, CWD: proj, DirOK: true, Owner: "qiushi", OwnerState: sessions.OwnerHistory}
	ui.listing.Sessions = []*sessions.Session{sess}
	ui.rows = ui.listing.Sessions
	keepWorkingDir(t)
	if done, _ := ui.commitResume(true); done || rec.called || !strings.Contains(ui.message, "already there") {
		t.Fatalf("x with one account: done %v called %v message %q", done, rec.called, ui.message)
	}
}

// Codex places its own launches: its own rows, its own record — and no busy
// sessions, no session routing, and nothing of Claude Code's.
func TestCodexPlacesItsOwnLaunches(t *testing.T) {
	f := newCodexFixture(t, "http://127.0.0.1:1/usage")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	refreshes := 0
	place := quietPlace()
	place.Refresh = func(config.Scope) error { refreshes++; return nil }

	f.extra(t, login(1))
	f.extra(t, login(2))
	f.extra(t, login(3))
	// A home whose auth.json holds no tokens: never observed, so the emptiest
	// of all, and no login a launch could run on.
	codexauthtest.WriteRaw(t, f.extra(t, login(4)), []byte(`{"auth_mode":"chatgpt"}`))
	st := state.Open(f.scope)
	store := func(n, percent int, edit ...[2]string) {
		t.Helper()
		l := login(n)
		body := codexUsage(l.AccountID, l.UserID, percent)
		for _, e := range edit {
			if !strings.Contains(body, e[0]) {
				t.Fatalf("the usage fixture has no %s to replace", e[0])
			}
			body = strings.Replace(body, e[0], e[1], 1)
		}
		k := state.Key{UUID: "uuid:" + l.AccountID + "/" + l.UserID, Name: l.Email}
		for _, a := range accounts.Discover(f.scope).Accounts {
			if a.Name == l.Email {
				k = state.Key{UUID: a.AccountID, Name: a.Name}
			}
		}
		// Past the quiet period, so a launch's refresh has someone to ask.
		at := time.Now().Add(-3 * time.Minute)
		dec, err := st.Claim([]state.Key{k}, at)
		if err != nil || !dec[0].Permit {
			t.Fatalf("claim: %+v %v", dec, err)
		}
		if _, err := st.Complete(k, dec[0].Generation, state.OutcomeStored, []byte(body), at); err != nil {
			t.Fatal(err)
		}
	}
	// A spent window that meters one feature — code review, an additional
	// limit — leaves the account usable for ordinary work, so it decides
	// nothing here: u2 carries both and is still the least loaded.
	spent := fmt.Sprintf(`{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,`+
		`"limit_window_seconds":604800,"reset_after_seconds":1000,"reset_at":%d},"secondary_window":null}`,
		time.Now().Add(72*time.Hour).Unix())
	store(1, 25)
	store(2, 12, [2]string{`"spend_control"`, `"code_review_rate_limit":` + spent +
		`,"additional_rate_limits":[{"limit_name":"spark","metered_feature":"spark","rate_limit":` + spent + `}],"spend_control"`})
	store(3, 0, [2]string{`"limit_reached":false`, `"limit_reached":true`}) // the emptiest, and the vendor refuses work on it

	launch := func(args ...string) (string, string) {
		t.Helper()
		got := launchOn(t, f.scope, place, args...)
		if got.code != 0 || !got.called {
			t.Fatalf("exit %d called %v: %s", got.code, got.called, got.stderr)
		}
		home, _ := envValue(got.env, "CODEX_HOME")
		return filepath.Base(home), got.stderr
	}
	first, stderr := launch("--", "resume", "--all")
	if first != "u2@x.com" {
		t.Fatalf("launched on %q, want u2@x.com (u3 is blocked, u1 is fuller):\n%s", first, stderr)
	}
	if !strings.Contains(stderr, "· auto ·") || strings.Contains(stderr, "session") {
		t.Errorf("a Codex `resume` is placed like a new session and says nothing of one:\n%s", stderr)
	}
	// That launch is one step of load on u2 until the vendor's figure catches
	// up with it, which puts it level with u1's two steps of usage; the tie
	// goes to the account not placed on. Without the pending step this is u2
	// again.
	if second, stderr := launch(); second != "u1@x.com" {
		t.Fatalf("the second launch went to %q, want u1@x.com: the first one's pending load did not count\n%s", second, stderr)
	}
	// Why the others were not chosen, as the dry run says it: a block and an
	// unreadable login are positive evidence, and Codex reports no busy
	// sessions.
	table := launchOn(t, f.scope, place, "--dry-run").stdout
	for _, want := range []string{"blocked by the vendor", "login unreadable"} {
		if !strings.Contains(table, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, table)
		}
	}
	recs, _, _ := readLog(t, f.scope)
	if recs[0].Vendor != "codex" || recs[0].Reason != placement.ReasonLeastLoad {
		t.Errorf("log = %+v", recs[0])
	}
	// Each choice the rule made leaves a refresh for the next one, as a
	// Claude Code launch's does; a launch on a named account chose nothing.
	if refreshes != 2 {
		t.Errorf("%d refreshes after two automatic launches, want 2", refreshes)
	}
	if named, stderr := launch("--account", "u1@x.com"); named != "u1@x.com" || refreshes != 2 {
		t.Errorf("a named launch went to %q and left %d refreshes in all:\n%s", named, refreshes, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Claude.AccountsRoot, "state.json")); err == nil {
		t.Error("a Codex launch wrote Claude Code's state.json")
	}
}
