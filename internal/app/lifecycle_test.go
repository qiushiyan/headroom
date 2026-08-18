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
	if code := run(deps); code != 2 {
		t.Errorf("no name: exit %d", code)
	}
	if code := run(deps, "a@x.com", "--bogus"); code != 2 {
		t.Errorf("bogus flag: exit %d", code)
	}
	// Off a terminal without --yes: refuse, touch nothing.
	if code := run(deps, "a@x.com"); code != 2 || len(keychainCalls) != 0 {
		t.Errorf("non-interactive without --yes: exit %d, keychain calls %v", code, keychainCalls)
	}
	// Interactive: the wrong reply aborts, the right one proceeds.
	inter := deps
	inter.interactive = true
	inter.stdin = strings.NewReader("wrong\n")
	if code := run(inter, "a@x.com"); code != 1 || len(keychainCalls) != 0 {
		t.Errorf("wrong confirmation: exit %d, keychain calls %v", code, keychainCalls)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatal("dir removed despite aborted confirmation")
	}

	// A live (or unverifiable) registered session refuses before anything.
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir, "sessions", "1.json"), []byte(`{"sessionId":"s1","pid":4242,"startedAt":1000000}`), 0o644)
	live := deps
	live.probe = func(int) (int64, bool) { return 1000, true } // matches startedAt/1000
	if code := run(live, "a@x.com", "--yes"); code != 1 || !strings.Contains(errw.String(), "live session") || len(keychainCalls) != 0 {
		t.Errorf("live: exit %d %s keychain=%v", code, errw.String(), keychainCalls)
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
	// Lock debris goes through the same door.
	os.Mkdir(filepath.Join(cfg.AccountsRoot, "b@x.com.lock"), 0o755)
	if code := run(deps, "b@x.com.lock", "--yes"); code != 0 {
		t.Errorf("lock debris: exit %d %s", code, errw.String())
	}
}
