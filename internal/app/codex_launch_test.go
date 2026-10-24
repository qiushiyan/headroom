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
	scope := config.ForHome(t.TempDir()).Codex
	scope.Present = true
	if _, _, _, err := accounts.Seed(scope, "cx@x.com", accounts.SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	return scope
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
	t.Setenv("CODEX_HOME", "/leaked/home")
	t.Setenv("CODEX_SQLITE_HOME", "/leaked/sqlite")
	t.Setenv("CODEX_API_KEY", "sk-secret-value")
	t.Setenv("CODEX_ACCESS_TOKEN", "tok-secret-value")
	t.Setenv("CLAUDE_CONFIG_DIR", "/accts/claude")

	got := launchOn(t, scope, quietPlace(), "--account", "cx@x.com", "--", "resume", "--all")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	stderr, env := got.stderr, got.env
	if !got.called || got.binary != "codex" || !slices.Equal(got.args, []string{"resume", "--all"}) {
		t.Fatalf("exec: called=%v argv0=%q args=%v", got.called, got.binary, got.args)
	}
	want := filepath.Join(scope.AccountsRoot, "cx@x.com")
	if v, ok := envValue(env, "CODEX_HOME"); !ok || v != want {
		t.Errorf("CODEX_HOME = %q, %v; want %s", v, ok, want)
	}
	for _, name := range []string{"CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if _, ok := envValue(env, name); ok {
			t.Errorf("%s survived into the child", name)
		}
	}
	if v, _ := envValue(env, "CLAUDE_CONFIG_DIR"); v != "/accts/claude" {
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
	primary := launchOn(t, scope, quietPlace(), "--account", "primary")
	if primary.code != 0 {
		t.Fatalf("primary launch exit %d: %s", primary.code, primary.stderr)
	}
	if _, ok := envValue(primary.env, "CODEX_HOME"); ok || !primary.called {
		t.Errorf("primary launch: CODEX_HOME present=%v called=%v", ok, primary.called)
	}

	// An inherited home naming a discovered extra is the ordinary "this shell
	// lives inside a managed session" case and stays quiet.
	t.Setenv("CODEX_HOME", want)
	for _, v := range []string{"CODEX_SQLITE_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		os.Unsetenv(v)
	}
	// The launch still says where it went; what stays unsaid is the inherited
	// variable.
	if quiet := launchOn(t, scope, quietPlace(), "--account", "primary").stderr; strings.Contains(quiet, "ignoring inherited") || !strings.Contains(quiet, "primary · named") {
		t.Errorf("a known extra's home was reported: %q", quiet)
	}
}

// --remember records Codex's .current, under Codex's root.
func TestCodexLaunchRemember(t *testing.T) {
	scope := codexLaunchFixture(t)
	if got := launchOn(t, scope, quietPlace(), "--remember", "--account", "cx@x.com"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
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
	if code := addTo(&out, &errw, scope, "new@x.com"); code != 0 {
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
	if code := addTo(&out, &errw, scope, "new@x.com"); code != 1 {
		t.Errorf("seeding over an existing home: exit %d", code)
	}
}

// Removing a Codex account refuses, with and without --yes, names the
// directory, and leaves it in place.
func TestCodexAccountsRemoveRefuses(t *testing.T) {
	scope := codexLaunchFixture(t)
	dir := filepath.Join(scope.AccountsRoot, "cx@x.com")
	for _, c := range []struct {
		name string
		yes  bool
	}{{"cx@x.com", false}, {"cx@x.com", true}, {"", true}, {"", false}} {
		got := call(func(std Streams) error {
			return AccountsRemove(std, Terminal{}, scope, c.name, c.yes, quietPlace().Probe)
		})
		if got.code != 1 {
			t.Errorf("%+v: exit %d, want 1", c, got.code)
		}
		// The remedy is the token a person acts on; the reason around it is prose.
		if !strings.Contains(got.stderr, "by hand") {
			t.Errorf("%+v: %s", c, got.stderr)
		}
		if c.name != "" && !strings.Contains(got.stderr, dir) {
			t.Errorf("%+v: the refusal does not name %s: %s", c, dir, got.stderr)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%+v: the directory is gone", c)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("the directory is gone")
	}
}
