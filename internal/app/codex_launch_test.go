package app

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

// codexLaunchFixture is a Codex tree with one seeded extra and a stub codex
// on PATH.
func codexLaunchFixture(t *testing.T) config.Scope {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, v := range []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CLAUDE_CONFIG_DIR"} {
		os.Unsetenv(v)
	}
	scope := config.ForHome(t.TempDir()).Codex
	scope.Present = true
	if _, _, err := accounts.Seed(scope, "cx@x.com", accounts.SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	return scope
}

// recordExec captures the exec edge: path, argv[0], args, environment.
func recordExec(t *testing.T) (binary *string, args, env *[]string, called *bool) {
	t.Helper()
	var b string
	var a, e []string
	var c bool
	prev := execVendor
	execVendor = func(path, bin string, childArgs, environ []string) error {
		b, a, e, c = bin, childArgs, environ, true
		return nil
	}
	t.Cleanup(func() { execVendor = prev })
	return &b, &a, &e, &c
}

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Launching Codex on a chosen account: codex is exec'd under argv[0] codex,
// CODEX_HOME is set for an extra and absent for the primary, every inherited
// variable that could re-route the child or substitute its credentials is
// removed and said on stderr — a credential's value never — and Claude Code's
// variables are left alone.
func TestCodexLaunchEnvironment(t *testing.T) {
	scope := codexLaunchFixture(t)
	binary, args, env, called := recordExec(t)
	t.Setenv("CODEX_HOME", "/leaked/home")
	t.Setenv("CODEX_SQLITE_HOME", "/leaked/sqlite")
	t.Setenv("CODEX_API_KEY", "sk-secret-value")
	t.Setenv("CODEX_ACCESS_TOKEN", "tok-secret-value")
	t.Setenv("CLAUDE_CONFIG_DIR", "/accts/claude")

	stderr := captureStderr(t, func() {
		if code := runLaunch(scope, []string{"--account", "cx@x.com", "--", "resume", "--all"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	if !*called || *binary != "codex" || !slices.Equal(*args, []string{"resume", "--all"}) {
		t.Fatalf("exec: called=%v argv0=%q args=%v", *called, *binary, *args)
	}
	want := filepath.Join(scope.AccountsRoot, "cx@x.com")
	if v, ok := envValue(*env, "CODEX_HOME"); !ok || v != want {
		t.Errorf("CODEX_HOME = %q, %v; want %s", v, ok, want)
	}
	for _, name := range []string{"CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if _, ok := envValue(*env, name); ok {
			t.Errorf("%s survived into the child", name)
		}
	}
	if v, _ := envValue(*env, "CLAUDE_CONFIG_DIR"); v != "/accts/claude" {
		t.Errorf("a Codex launch touched Claude Code's variable: %q", v)
	}
	for _, name := range []string{"CODEX_HOME=/leaked/home", "CODEX_SQLITE_HOME=/leaked/sqlite", "CODEX_API_KEY (value not shown)", "CODEX_ACCESS_TOKEN (value not shown)"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("stderr does not name %q:\n%s", name, stderr)
		}
	}
	if strings.Contains(stderr, "secret-value") {
		t.Errorf("a credential value reached stderr:\n%s", stderr)
	}

	// The primary is selected by absence.
	*called = false
	captureStderr(t, func() {
		if code := runLaunch(scope, []string{"--account", "primary"}); code != 0 {
			t.Fatalf("primary launch exit %d", code)
		}
	})
	if _, ok := envValue(*env, "CODEX_HOME"); ok || !*called {
		t.Errorf("primary launch: CODEX_HOME present=%v called=%v", ok, *called)
	}

	// An inherited home naming a discovered extra is the ordinary "this shell
	// lives inside a managed session" case and stays quiet.
	t.Setenv("CODEX_HOME", want)
	for _, v := range []string{"CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		os.Unsetenv(v)
	}
	if quiet := captureStderr(t, func() { runLaunch(scope, []string{"--account", "primary"}) }); quiet != "" {
		t.Errorf("a known extra's home was reported: %q", quiet)
	}
}

// Launch refuses before .current is written: a sessions/ that is not the
// shared link, and the primary under a re-pointed HEADROOM_HOME.
func TestCodexLaunchRefusesBeforeRecording(t *testing.T) {
	scope := codexLaunchFixture(t)
	_, _, _, called := recordExec(t)

	link := filepath.Join(scope.AccountsRoot, "cx@x.com", "sessions")
	os.Remove(link)
	os.MkdirAll(link, 0o755) // a real directory: the history fork
	stderr := captureStderr(t, func() {
		if code := runLaunch(scope, []string{"--remember", "--account", "cx@x.com"}); code != 1 {
			t.Errorf("broken sessions link: exit %d", code)
		}
	})
	if !strings.Contains(stderr, "not launching") || !strings.Contains(stderr, "sessions") {
		t.Errorf("stderr = %q", stderr)
	}

	relocated := scope
	relocated.PrimaryRelocated = true
	stderr = captureStderr(t, func() {
		if code := runLaunch(relocated, []string{"--remember", "--account", "primary"}); code != 1 {
			t.Errorf("relocated primary: exit %d", code)
		}
	})
	if !strings.Contains(stderr, "HEADROOM_HOME") {
		t.Errorf("stderr = %q", stderr)
	}
	if *called {
		t.Error("exec ran through a refusal")
	}
	if _, err := os.Stat(scope.CurrentFile()); err == nil {
		t.Error(".current was written before the refusal")
	}
}

// --remember records Codex's .current, under Codex's root.
func TestCodexLaunchRemember(t *testing.T) {
	scope := codexLaunchFixture(t)
	recordExec(t)
	captureStderr(t, func() {
		if code := runLaunch(scope, []string{"--remember", "--account", "cx@x.com"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	if got, _ := os.ReadFile(scope.CurrentFile()); string(got) != "cx@x.com\n" {
		t.Errorf(".current = %q", got)
	}
	if a, err := accounts.Discover(scope).Select(""); err != nil || a.Name != "cx@x.com" {
		t.Errorf("bare Codex launch resolves to %v, %v", a.Name, err)
	}
}

// accounts add --vendor codex: the link, and a log-in hint in the engine's own
// spelling that never says /login.
func TestCodexAccountsAdd(t *testing.T) {
	scope := config.ForHome(t.TempDir()).Codex // absent: add is how Codex comes to exist
	var out, errw bytes.Buffer
	if code := runAccountsAddTo(&out, &errw, scope, []string{"new@x.com"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	text := out.String()
	for _, want := range []string{
		"sessions → " + scope.StoreDir(),
		"headroom launch --vendor codex --account new@x.com -- login",
		scope.OrderFile(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "/login") || strings.Contains(text, "projects") {
		t.Errorf("Codex add output speaks Claude Code:\n%s", text)
	}
	if code := runAccountsAddTo(&out, &errw, scope, []string{"new@x.com"}); code != 1 {
		t.Errorf("seeding over an existing home: exit %d", code)
	}
}

// Removing a Codex account refuses, with and without --yes, names the
// directory, and leaves it in place.
func TestCodexAccountsRemoveRefuses(t *testing.T) {
	scope := codexLaunchFixture(t)
	dir := filepath.Join(scope.AccountsRoot, "cx@x.com")
	for _, args := range [][]string{{"cx@x.com"}, {"cx@x.com", "--yes"}, {"--yes", "cx@x.com"}, {}} {
		var errw bytes.Buffer
		if code := refuseCodexRemove(&errw, scope, args); code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		msg := errw.String()
		if !strings.Contains(msg, "cannot tell whether a Codex session is running") || !strings.Contains(msg, "by hand") {
			t.Errorf("%v: %s", args, msg)
		}
		if len(args) > 0 && args[0] != "--yes" || len(args) == 2 {
			if !strings.Contains(msg, dir) {
				t.Errorf("%v: the refusal does not name %s: %s", args, dir, msg)
			}
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%v: the directory is gone", args)
		}
	}
	// And through the command's own entry point.
	stderr := captureStderr(t, func() {
		if code := runAccountsRemove(scope, []string{"cx@x.com", "--yes"}); code != 1 {
			t.Errorf("exit %d", code)
		}
	})
	if !strings.Contains(stderr, dir) {
		t.Errorf("stderr = %q", stderr)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("the directory is gone")
	}
}
