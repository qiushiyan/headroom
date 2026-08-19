package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

func lifecycleConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAccountsAddCommand(t *testing.T) {
	cfg := lifecycleConfig(t)
	var out, errw bytes.Buffer
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"a@x.com"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "headroom launch --account a@x.com") || !strings.Contains(out.String(), "/login") {
		t.Errorf("next step not printed:\n%s", out.String())
	}
	if err := accounts.VerifyTopology(cfg, accounts.Account{ConfigDir: filepath.Join(cfg.AccountsRoot, "a@x.com"), Name: "a@x.com"}); err != nil {
		t.Fatal(err)
	}
	// Flag policy: no name, unknown flag, extra arg, empty --share-config=.
	for _, args := range [][]string{{}, {"a@x.com", "--bogus"}, {"a@x.com", "b@x.com"}, {"b@x.com", "--share-config="}} {
		errw.Reset()
		if code := runAccountsAddTo(&out, &errw, cfg, args); code != 2 {
			t.Errorf("args %v: exit %d, want 2 (%s)", args, code, errw.String())
		}
	}
	// A seeding failure is exit 1, and says why.
	errw.Reset()
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"a@x.com"}); code != 1 || !strings.Contains(errw.String(), "already exists") {
		t.Errorf("re-seed: exit %d, %s", code, errw.String())
	}
	// Bare --share-config uses the primary's whitelist.
	os.WriteFile(filepath.Join(cfg.PrimaryDir(), "settings.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(cfg.PrimaryDir(), "history.jsonl"), []byte(""), 0o644)
	out.Reset()
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"--share-config", "c@x.com"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "shared from "+cfg.PrimaryDir()+": settings.json") {
		t.Errorf("share report:\n%s", out.String())
	}
	if _, err := os.Lstat(filepath.Join(cfg.AccountsRoot, "c@x.com", "history.jsonl")); err == nil {
		t.Error("history.jsonl shared")
	}
	// --share-config=<dir> links every entry of that dir; a relative value is
	// resolved against the cwd before it reaches the engine.
	pkg := filepath.Join(cfg.Home, "pkg")
	os.MkdirAll(filepath.Join(pkg, "hooks"), 0o755)
	os.WriteFile(filepath.Join(pkg, "anything.md"), []byte(""), 0o644)
	out.Reset()
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"d@x.com", "--share-config=" + pkg}); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "anything.md") || !strings.Contains(out.String(), "hooks") {
		t.Errorf("whole-dir share report:\n%s", out.String())
	}
	wd, _ := os.Getwd()
	os.Chdir(cfg.Home)
	defer os.Chdir(wd)
	out.Reset()
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"e@x.com", "--share-config=pkg"}); code != 0 {
		t.Fatalf("relative --share-config: exit %d: %s", code, errw.String())
	}
	// Compared by identity: t.TempDir lives under /var, which is itself a
	// symlink on macOS, so the resolved absolute path spells differently.
	want, _ := os.Stat(filepath.Join(pkg, "anything.md"))
	got, err := os.Stat(filepath.Join(cfg.AccountsRoot, "e@x.com", "anything.md"))
	if err != nil || !os.SameFile(want, got) {
		t.Errorf("relative share source did not resolve to the package file (%v)", err)
	}
}

// A canonical store that is a regular file is as much a topology violation
// as a symlinked one; seeding refuses and creates nothing.
func TestAccountsAddRefusesFileAsStore(t *testing.T) {
	cfg := lifecycleConfig(t)
	os.WriteFile(cfg.ProjectsDir(), []byte("not a dir"), 0o644)
	var out, errw bytes.Buffer
	if code := runAccountsAddTo(&out, &errw, cfg, []string{"a@x.com"}); code != 1 || !strings.Contains(errw.String(), "not a directory") {
		t.Errorf("exit %d: %s", code, errw.String())
	}
	if _, err := os.Lstat(filepath.Join(cfg.AccountsRoot, "a@x.com")); err == nil {
		t.Error("dir created")
	}
}

// The dispatch: `accounts add|remove` route to the lifecycle commands, and
// the board (`accounts`, bare, `select`) still refuses any argument.
func TestAccountsDispatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_PRIMARY_NAME", "")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	if code := Run([]string{"accounts", "add"}); code != 2 {
		t.Errorf("accounts add (no name): exit %d, want usage 2", code)
	}
	if code := Run([]string{"accounts", "add", "a@x.com"}); code != 0 {
		t.Errorf("accounts add: exit %d", code)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude-accounts", "a@x.com", "projects")); err != nil {
		t.Errorf("accounts add did not seed through the dispatch: %v", err)
	}
	if code := Run([]string{"accounts", "remove"}); code != 2 {
		t.Errorf("accounts remove (no name): exit %d, want usage 2", code)
	}
	if code := Run([]string{"accounts", "bogus"}); code != 2 {
		t.Errorf("accounts bogus: exit %d, want 2", code)
	}
	if code := Run([]string{"select", "add", "a@x.com"}); code != 2 {
		t.Errorf("select add: exit %d, want 2 — only `accounts` carries subcommands", code)
	}
}

func TestAccountsRemoveCommand(t *testing.T) {
	cfg := lifecycleConfig(t)
	seed := func(name string) string {
		t.Helper()
		dir, _, err := accounts.Seed(cfg, name, accounts.SeedOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := seed("a@x.com")
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"a@x.com"}}`), 0o644)
	os.WriteFile(cfg.PrimaryMeta(), []byte(`{"oauthAccount":{"emailAddress":"p@x.com"}}`), 0o644)

	var out, errw bytes.Buffer
	var keychainCalls []string
	deps := removeDeps{
		probe:          func(int) (int64, bool) { return 0, false }, // every pid is dead
		deleteKeychain: func(d string) (bool, error) { keychainCalls = append(keychainCalls, d); return true, nil },
		stdin:          strings.NewReader(""),
		interactive:    false,
	}
	run := func(d removeDeps, args ...string) int {
		out.Reset()
		errw.Reset()
		return runAccountsRemoveTo(&out, &errw, cfg, args, d)
	}

	// The primary refuses by name; a missing dir refuses; flag policy.
	if code := run(deps, "primary", "--yes"); code != 1 || !strings.Contains(errw.String(), "primary") {
		t.Errorf("primary: exit %d %s", code, errw.String())
	}
	if code := run(deps, "nope@x.com", "--yes"); code != 1 {
		t.Errorf("missing: exit %d", code)
	}
	if code := run(deps); code != 2 || !strings.Contains(errw.String(), "removable:") || !strings.Contains(errw.String(), "a@x.com  (logged in)") {
		t.Errorf("no name off a terminal: exit %d %s", code, errw.String())
	}
	// A typo names what would have worked.
	if code := run(deps, "a@x.con", "--yes"); code != 1 || !strings.Contains(errw.String(), `no account named "a@x.con"`) || !strings.Contains(errw.String(), "a@x.com") {
		t.Errorf("typo: exit %d %s", code, errw.String())
	}
	if code := run(deps, "a@x.com", "--bogus"); code != 2 {
		t.Errorf("bogus flag: exit %d", code)
	}
	// Off a terminal without --yes: refuse, touch nothing.
	if code := run(deps, "a@x.com"); code != 2 || len(keychainCalls) != 0 {
		t.Errorf("non-interactive without --yes: exit %d, keychain calls %v", code, keychainCalls)
	}
	// Interactive: anything but y/yes aborts — including the name itself,
	// which the old prompt asked for.
	inter := deps
	inter.interactive = true
	for _, reply := range []string{"n\n", "a@x.com\n", "\n", ""} {
		inter.stdin = strings.NewReader(reply)
		if code := run(inter, "a@x.com"); code != 1 || len(keychainCalls) != 0 || !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("reply %q: exit %d, keychain calls %v, out %q", reply, code, keychainCalls, out.String())
		}
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatal("dir removed despite aborted confirmation")
	}
	// Bare invocation on a terminal: the picker chooses and confirms;
	// cancelling aborts, and a confirmed choice still meets the gate
	// (exercised below with a@x.com's live session) before anything runs.
	var picked []removeCandidate
	bare := inter
	bare.pick = func(c []removeCandidate) (string, bool) { picked = c; return "", false }
	if code := run(bare); code != 1 || len(picked) != 1 || picked[0].Name != "a@x.com" || picked[0].Email != "a@x.com" {
		t.Errorf("picker cancel: exit %d, candidates %+v", code, picked)
	}

	// A live (or unverifiable) registered session refuses before anything.
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir, "sessions", "1.json"), []byte(`{"sessionId":"s1","pid":4242,"startedAt":1000000}`), 0o644)
	live := deps
	live.probe = func(int) (int64, bool) { return 1000, true } // matches startedAt/1000
	if code := run(live, "a@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "live session") || len(keychainCalls) != 0 {
		t.Errorf("live: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
	}
	pickedLive := live
	pickedLive.interactive = true
	pickedLive.pick = func([]removeCandidate) (string, bool) { return "a@x.com", true }
	if code := run(pickedLive); code != 1 || !strings.Contains(errw.String(), "live session") || len(keychainCalls) != 0 {
		t.Errorf("picked live: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
	}
	unknown := deps
	unknown.probe = nil
	if code := run(unknown, "a@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "could not be verified") {
		t.Errorf("unverifiable: exit %d %s", code, errw.String())
	}
	// Dead pid: the claim is stale, removal proceeds — Keychain first, then dir.
	os.WriteFile(cfg.CurrentFile(), []byte("a@x.com\n"), 0o644)
	os.WriteFile(filepath.Join(cfg.ProjectsDir(), "t.jsonl"), []byte("x"), 0o644)
	if code := run(deps, "a@x.com", "--yes"); code != 0 {
		t.Fatalf("remove: exit %d %s", code, errw.String())
	}
	if len(keychainCalls) != 1 || keychainCalls[0] != dir {
		t.Errorf("keychain calls %v", keychainCalls)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("dir still exists")
	}
	if _, err := os.Stat(filepath.Join(cfg.ProjectsDir(), "t.jsonl")); err != nil {
		t.Error("store contents deleted")
	}
	if !strings.Contains(out.String(), "deleted Keychain item") || !strings.Contains(out.String(), ".current pointed here") {
		t.Errorf("report:\n%s", out.String())
	}
	if got, _ := os.ReadFile(cfg.CurrentFile()); string(got) != "a@x.com\n" {
		t.Errorf(".current rewritten: %q", got)
	}

	// A Keychain failure leaves the dir for a retry.
	dir2 := seed("b@x.com")
	failing := deps
	failing.deleteKeychain = func(string) (bool, error) { return false, os.ErrPermission }
	if code := run(failing, "b@x.com", "--yes"); code != 1 {
		t.Errorf("keychain failure: exit %d", code)
	}
	if _, err := os.Lstat(dir2); err != nil {
		t.Error("dir removed although the Keychain step failed")
	}
	// An email account that never logged in (no .claude.json) removes cleanly.
	seed("c@x.com")
	noItem := deps
	noItem.deleteKeychain = func(string) (bool, error) { return false, nil }
	if code := run(noItem, "c@x.com", "--yes"); code != 0 || !strings.Contains(out.String(), "no Keychain item") {
		t.Errorf("never-logged-in: exit %d %s", code, errw.String())
	}
	// A registry file that does not parse is "could not verify", not
	// "nothing live"; so is a sessions/ dir that cannot be listed.
	dir3 := seed("d@x.com")
	os.MkdirAll(filepath.Join(dir3, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir3, "sessions", "junk.json"), []byte("{not json"), 0o644)
	if code := run(deps, "d@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "could not be verified") {
		t.Errorf("malformed registry: exit %d %s", code, errw.String())
	}
	if _, err := os.Lstat(dir3); err != nil {
		t.Error("dir removed over an unverifiable registry")
	}
	os.Remove(filepath.Join(dir3, "sessions", "junk.json"))
	os.Chmod(filepath.Join(dir3, "sessions"), 0o000)
	if code := run(deps, "d@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "registry unreadable") {
		t.Errorf("unreadable registry: exit %d %s", code, errw.String())
	}
	os.Chmod(filepath.Join(dir3, "sessions"), 0o755)
	// A real projects/ directory (unmigrated sessions) refuses before the
	// Keychain step.
	os.Remove(filepath.Join(dir3, "projects"))
	os.MkdirAll(filepath.Join(dir3, "projects", "p"), 0o755)
	keychainCalls = nil
	if code := run(deps, "d@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "real directory") || len(keychainCalls) != 0 {
		t.Errorf("unmigrated projects: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
	}
	// A session that starts while the confirmation prompt is open refuses:
	// the gate runs again after the reply, before the Keychain step.
	dir4 := seed("e@x.com")
	os.MkdirAll(filepath.Join(dir4, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir4, "sessions", "1.json"), []byte(`{"sessionId":"s4","pid":4343,"startedAt":1000000}`), 0o644)
	calls := 0
	racing := deps
	racing.interactive = true
	racing.stdin = strings.NewReader("y\n")
	racing.probe = func(int) (int64, bool) {
		calls++
		if calls == 1 {
			return 0, false // dead at the first gate
		}
		return 1000, true // alive by the time the reply arrives
	}
	keychainCalls = nil
	if code := run(racing, "e@x.com"); code != 1 || !strings.Contains(errw.String(), "live session") || len(keychainCalls) != 0 {
		t.Errorf("session started during prompt: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
	}
	// A confirmed picker choice proceeds straight to removal — no second
	// prompt reads stdin (EOF here), the dir goes.
	dir5 := seed("f@x.com")
	pickedOK := deps
	pickedOK.interactive = true
	pickedOK.stdin = strings.NewReader("")
	pickedOK.pick = func([]removeCandidate) (string, bool) { return "f@x.com", true }
	keychainCalls = nil
	if code := run(pickedOK); code != 0 || len(keychainCalls) != 1 {
		t.Errorf("picked dead account: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
	}
	if _, err := os.Lstat(dir5); err == nil {
		t.Error("picked dir still exists")
	}
	// Lock debris goes through the same door, is offered by the picker,
	// and "yes" confirms.
	os.Mkdir(filepath.Join(cfg.AccountsRoot, "b@x.com.lock"), 0o755)
	cands, cerr := removeCandidates(cfg)
	if cerr != nil || len(cands) == 0 || !cands[len(cands)-1].Lock || cands[len(cands)-1].Name != "b@x.com.lock" {
		t.Errorf("candidates %+v (%v)", cands, cerr)
	}
	yes := deps
	yes.interactive = true
	yes.stdin = strings.NewReader("YES\n")
	if code := run(yes, "b@x.com.lock"); code != 0 {
		t.Errorf("lock debris: exit %d %s", code, errw.String())
	}

	// Nothing removable: the bare terminal invocation says so and never
	// opens the picker; an unreadable root is reported as such rather than
	// as an empty one.
	cands, _ = removeCandidates(cfg)
	for _, c := range cands {
		// Test fixtures, not accounts: the unmigrated-projects dir above
		// would (rightly) make RemoveDir refuse.
		os.RemoveAll(filepath.Join(cfg.AccountsRoot, c.Name))
	}
	empty := deps
	empty.interactive = true
	empty.pick = func([]removeCandidate) (string, bool) { t.Fatal("picker opened over no candidates"); return "", false }
	if code := run(empty); code != 2 || !strings.Contains(errw.String(), "nothing to remove") {
		t.Errorf("no candidates: exit %d %s", code, errw.String())
	}
	os.Chmod(cfg.AccountsRoot, 0o000)
	defer os.Chmod(cfg.AccountsRoot, 0o755)
	if code := run(empty); code != 2 || strings.Contains(errw.String(), "nothing to remove") || !strings.Contains(errw.String(), "listing") {
		t.Errorf("unreadable root: exit %d %s", code, errw.String())
	}
	os.Chmod(cfg.AccountsRoot, 0o755)
}
