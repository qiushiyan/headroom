package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// reservedFixture has three accounts where the one kept for the repository is
// the one the rule would choose anywhere: held has the most weekly room, so
// only a reservation can keep it out. It returns the repository's main
// checkout, a worktree of it and a directory outside it.
func reservedFixture(t *testing.T) (f autoFixture, repo, worktree, outside string) {
	t.Helper()
	f = newAutoFixture(t, "free@x.com", "held@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 50, 50, time.Minute)
	f.observe("free@x.com", 0, 70, time.Minute)
	f.observe("held@x.com", 0, 10, time.Minute)

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, worktree, outside = filepath.Join(base, "repo"), filepath.Join(base, "trees", "feat"), filepath.Join(base, "other")
	for _, dir := range []string{filepath.Join(repo, ".git", "worktrees", "feat"), filepath.Join(repo, "apps"), filepath.Join(worktree, "apps"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(repo, ".git", "HEAD"):                      "ref: refs/heads/main\n",
		filepath.Join(repo, ".git", "worktrees", "feat", "HEAD"): "ref: refs/heads/feat\n",
		filepath.Join(worktree, ".git"):                          "gitdir: " + filepath.Join(repo, ".git", "worktrees", "feat") + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.SetReservation(f.cfg, "held@x.com", []string{repo}); err != nil {
		t.Fatal(err)
	}
	return f, repo, worktree, outside
}

// marked is the account the board's mark names for a launch in dir, read the
// way the board reads it.
func (f autoFixture) marked(dir string) string {
	f.t.Helper()
	set, now := accounts.Discover(f.cfg), time.Now()
	accts, _ := accountstate.Assemble(set, f.st.Load(), now)
	for name, m := range launchop.Mark(set, f.st, accts, "auto", os.Environ(), dir, now, f.edges).At(now) {
		if m.Next {
			return name
		}
	}
	return ""
}

// Every automatic surface decides one way for one directory: the launch, the
// board's mark and a headless placement, in the repository, in a worktree of
// it, and outside it.
func TestEveryAutomaticSurfaceHonoursAReservation(t *testing.T) {
	f, repo, worktree, outside := reservedFixture(t)
	f.mint("free@x.com", 30*24*time.Hour)
	f.mint("held@x.com", 30*24*time.Hour)
	for _, c := range []struct {
		dir, want string
	}{
		{filepath.Join(repo, "apps"), "held@x.com"},
		{filepath.Join(worktree, "apps"), "held@x.com"},
		{outside, "free@x.com"},
	} {
		t.Chdir(c.dir)
		if got := f.marked(c.dir); got != c.want {
			t.Errorf("mark in %s = %q, want %s", c.dir, got, c.want)
		}
		doc, code, _ := f.place("--dry-run")
		if code != 0 || doc.Account != c.want {
			t.Errorf("headless place in %s = %q (exit %d), want %s", c.dir, doc.Account, code, c.want)
		}
		l := f.launch("--dry-run")
		if l.code != 0 || !strings.Contains(l.stdout, "on "+c.want) {
			t.Errorf("launch in %s:\n%s%s", c.dir, l.stdout, l.stderr)
		}
	}

	// The board says what an account is kept for wherever it was opened, and
	// its document says nothing new: that is `accounts reserve`'s to list.
	set, now := accounts.Discover(f.cfg), time.Now()
	accts, _ := accountstate.Assemble(set, f.st.Load(), now)
	marks := launchop.Mark(set, f.st, accts, "auto", os.Environ(), outside, now, f.edges).At(now)
	if got := marks["held@x.com"].Reserved; got != "reserved for "+repo {
		t.Errorf("the held account's mark says %q", got)
	}
	if got := marks["free@x.com"].Reserved; got != "" {
		t.Errorf("the free account's mark says %q", got)
	}

	t.Chdir(outside)
	if doc, _, _ := f.place("--dry-run"); !strings.Contains(doc.find("held@x.com").Excluded, "reserved for") {
		t.Errorf("held outside its repository: %+v", doc.find("held@x.com"))
	}
	l := f.launch()
	if rec := f.log()[0]; l.account(f.cfg) != "free@x.com" || rec.Candidates[2].Reservation != "elsewhere" || rec.Candidates[2].Eligible {
		t.Errorf("launch outside: %s\nlog: %+v", l, rec.Candidates[2])
	}
}

// In its repository the account is ranked like any other, so a parallel launch
// goes to any account with less load and every other account stays a candidate.
func TestInItsRepositoryAReservedAccountYieldsToLessLoad(t *testing.T) {
	f, repo, _, _ := reservedFixture(t)
	t.Chdir(repo)
	first := f.launch()
	if got := first.account(f.cfg); got != "held@x.com" {
		t.Fatalf("first launch: %s", first)
	}
	if got := f.launch().account(f.cfg); got != "free@x.com" {
		t.Errorf("a parallel launch went to %s, and every other account is still a candidate", got)
	}
}

// Named and pinned launches say where to go: a reservation governs automatic
// choices only.
func TestAReservationLeavesAnExplicitLaunchAlone(t *testing.T) {
	f, _, _, outside := reservedFixture(t)
	t.Chdir(outside)
	if got := f.launch("--account", "held@x.com").account(f.cfg); got != "held@x.com" {
		t.Errorf("named launch went to %s", got)
	}
	if got := f.launch("--last").account(f.cfg); got != "held@x.com" {
		t.Errorf("--last went to %s", got)
	}
	f.setCurrent("held@x.com\n")
	if got := f.launch().account(f.cfg); got != "held@x.com" {
		t.Errorf("pinned launch went to %s", got)
	}
}

// Losing a repository is not permission: the account stays out of everything
// else, and nothing names it as chosen.
func TestAMissingRepositoryKeepsItsAccountHeld(t *testing.T) {
	f, repo, _, outside := reservedFixture(t)
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outside)
	if l := f.launch(); l.code != 0 || l.account(f.cfg) == "held@x.com" {
		t.Fatalf("launch: %s", l)
	}
	held := f.log()[0].Candidates[2]
	if held.Name != "held@x.com" || held.Eligible || held.Reservation != "elsewhere" {
		t.Errorf("with its repository gone: %+v", held)
	}
}

// A reservation that cannot be read says nothing about which accounts are kept
// for what, so nothing automatic is decided from it, and a launch that names
// its account does not need to know.
func TestADamagedReservationRefusesOnlyAutomaticChoices(t *testing.T) {
	f, _, _, outside := reservedFixture(t)
	f.mint("free@x.com", 30*24*time.Hour)
	if err := os.WriteFile(f.cfg.ReservedFile(), []byte("not a row\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outside)
	before := len(f.log())

	if l := f.launch(); l.code == 0 || l.called || !strings.Contains(l.stderr, ".reserved line 1") {
		t.Errorf("automatic launch on a damaged file: %s", l)
	}
	if len(f.log()) != before {
		t.Error("a refused launch left a line in the log")
	}
	if got := f.marked(outside); got != "" {
		t.Errorf("the board marked %s as the next launch, and a launch refuses", got)
	}
	if doc, code, _ := f.place(); code != 1 || doc.Placed || !strings.Contains(doc.Refusal, ".reserved") {
		t.Errorf("headless place on a damaged file: exit %d %+v", code, doc)
	}
	if l := f.launch("--account", "free@x.com"); l.code != 0 || !l.called {
		t.Errorf("named launch on a damaged file: %s", l)
	}
}

// A session is not an account the person named: outside the repositories its
// owner is kept from, a resume moves it and records the move, on the CLI and
// in the picker alike. In them, and when bare launches are pinned, the owner is
// followed as it always was.
func TestAResumeLeavesAHeldOwnerOutsideItsRepositories(t *testing.T) {
	f, repo, _, outside := reservedFixture(t)
	for _, id := range []string{sessA, sessB} {
		transcript := filepath.Join(f.cfg.StoreDir(), "-proj", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(repo)
	if got := f.launch("--", "--resume", sessA).account(f.cfg); got != "held@x.com" {
		t.Fatalf("the session started on %s", got)
	}

	t.Chdir(outside)
	if got := f.launch("--", "--resume", sessA).account(f.cfg); got != "free@x.com" {
		t.Errorf("--resume went to %s", got)
	}
	if rec, _ := f.st.Load().Owner(sessA); rec.Account != "free@x.com" {
		t.Errorf("re-home after --resume = %+v", rec)
	}

	set := accounts.Discover(f.cfg)
	resume := func(routing accounts.Routing, dir string) (*execRecord, *resumeUI) {
		t.Helper()
		ui, _, _ := f.picker(set, routing)
		ui.listing.Sessions = []*sessions.Session{{ID: sessB, CWD: dir, DirOK: true, Owner: "held@x.com", OwnerState: sessions.OwnerHistory}}
		ui.rows = ui.listing.Sessions
		keepWorkingDir(t)
		rec := rearm(ui)
		if done, code := ui.commitResume(false); !done || code != 0 {
			t.Fatalf("resume in %s: (%v, %d) — %s", dir, done, code, ui.message)
		}
		return rec, ui
	}
	account := func(rec *execRecord) string {
		if dir, ok := envValue(rec.env, "CLAUDE_CONFIG_DIR"); ok {
			return filepath.Base(dir)
		}
		return "qiushi"
	}

	if rec, _ := resume(set.Routing(), repo); account(rec) != "held@x.com" {
		t.Errorf("a picker resume in the repository went to %s", account(rec))
	}
	if _, ok := f.st.Load().Owner(sessB); ok {
		t.Error("following the owner wrote a re-home")
	}
	if rec, _ := resume(accounts.Routing{Mode: "pinned", Current: "free@x.com"}, outside); account(rec) != "held@x.com" {
		t.Errorf("a pinned picker resume went to %s: only an automatic choice is kept out", account(rec))
	}
	if rec, _ := resume(set.Routing(), outside); account(rec) == "held@x.com" {
		t.Error("a picker resume outside the repository stayed on the held account")
	}
	if rec, ok := f.st.Load().Owner(sessB); !ok || rec.Account == "held@x.com" {
		t.Errorf("the picker's move was not recorded: %+v", rec)
	}
	if last := f.log()[len(f.log())-1]; last.Reason != placement.ReasonMoved {
		t.Errorf("the picker's move logged as %q", last.Reason)
	}
}

// The override never lands on an account a reservation keeps from the
// session's directory.
func TestTheOverrideCannotLandOnAHeldAccount(t *testing.T) {
	f, _, _, outside := reservedFixture(t)
	set := accounts.Discover(f.cfg)
	ui, _, _ := f.picker(set, set.Routing())
	ui.listing.Sessions = []*sessions.Session{{ID: sessA, CWD: outside, DirOK: true, Owner: "free@x.com", OwnerState: sessions.OwnerHistory}}
	ui.rows = ui.listing.Sessions
	keepWorkingDir(t)
	rec := rearm(ui)
	if done, code := ui.commitResume(true); !done || code != 0 {
		t.Fatalf("x: (%v, %d) — %s", done, code, ui.message)
	}
	if dir, ok := envValue(rec.env, "CLAUDE_CONFIG_DIR"); ok && filepath.Base(dir) == "held@x.com" {
		t.Error("x moved the session onto the held account")
	}
}
