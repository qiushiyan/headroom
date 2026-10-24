package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

func launchConfig(t *testing.T) config.Scope {
	t.Helper()
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := claudeScope(home, "qiushi")
	extra := filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid shared-sessions topology: launch verifies it before any exec.
	if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg.StoreDir(), filepath.Join(extra, "projects")); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The child names exactly the account the launch decided, under a shell that
// says otherwise: the wrong-account incident was a tmux server started inside
// a managed session routing every "primary" launch to that session's account.
// An inherited CLAUDE_CONFIG_DIR is stripped for the primary and replaced by
// exactly one for an extra, and a home that spells its primary out names that
// dir — which a re-pointed home does not refuse, since the refusal exists for
// selection by absence. Arguments after `--` reach the vendor unchanged. A
// pinned or named launch that names a session records nothing about it: it
// was told where to go, and a re-home written onto the pin would steer the
// session's next automatic turn.
func TestALaunchNamesExactlyItsAccount(t *testing.T) {
	for _, c := range []struct {
		name     string
		explicit bool // the home spells its primary out, and HEADROOM_HOME re-points it
		args     []string
		dirs     func(cfg config.Scope) []string
	}{
		{"the primary, pinned by absence", false, []string{"--", "--resume", sessA},
			func(config.Scope) []string { return nil }},
		{"an extra, named", false, []string{"--account", "yan@planlab.ai", "--", "--resume", sessA},
			func(cfg config.Scope) []string { return []string{filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")} }},
		{"a spelled-out primary under a re-pointed home", true, []string{"--account", "qiushi"},
			func(cfg config.Scope) []string { return []string{cfg.PrimaryDir()} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := launchConfig(t)
			cfg.PrimaryExplicit, cfg.PrimaryRelocated = c.explicit, c.explicit
			_, args, env, called := recordExec(t)
			t.Setenv("CLAUDE_CONFIG_DIR", "/leaked/other-account")
			var code int
			stderr := captureStderr(t, func() { code = runLaunch(cfg, c.args) })
			if code != 0 || !*called {
				t.Fatalf("exit %d, exec called %v: %s", code, *called, stderr)
			}
			if got, want := configDirs(*env), c.dirs(cfg); !slices.Equal(got, want) {
				t.Errorf("CLAUDE_CONFIG_DIR = %q, want exactly %q", got, want)
			}
			var rest []string
			if i := slices.Index(c.args, "--"); i >= 0 {
				rest = c.args[i+1:]
			}
			if !slices.Equal(*args, rest) {
				t.Errorf("vendor args = %q, want %q", *args, rest)
			}
			if rec, ok := state.Open(cfg).Load().Owner(sessA); ok {
				t.Errorf("a launch told where to go re-homed the session it named: %+v", rec)
			}
		})
	}
}

// resolve's one line is a protocol a shell wrapper parses: the canonical name,
// TAB, the real config dir, TAB, the kind. The primary resolves to its real
// dir, never an empty sentinel, and the kind is what the wrapper's preflight
// keys on — re-deriving primary-vs-extra from path prefixes is exactly what a
// HEADROOM_* root override silently breaks. What the framing cannot carry
// refuses: an empty selector is a caller's expansion bug that would otherwise
// answer with the recorded account, and a TAB or newline would split the line.
func TestResolveProtocol(t *testing.T) {
	cfg := launchConfig(t)
	for _, name := range []string{"tab\tname", "new\nline"} {
		if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"no .current: the primary", nil, 0, "qiushi\t" + cfg.PrimaryDir() + "\tprimary\n"},
		{"an extra", []string{"yan@planlab.ai"}, 0, "yan@planlab.ai\t" + filepath.Join(cfg.AccountsRoot, "yan@planlab.ai") + "\textra\n"},
		{"an empty selector", []string{""}, 2, ""},
		{"a name with a TAB", []string{"tab\tname"}, 1, ""},
		{"a name with a newline", []string{"new\nline"}, 1, ""},
	} {
		var code int
		out := captureStdout(t, func() { captureStderr(t, func() { code = runResolve(cfg, c.args) }) })
		if code != c.code || out != c.out {
			t.Errorf("%s: exit %d, %q; want exit %d, %q", c.name, code, out, c.code, c.out)
		}
	}
}

// A launch that cannot run refuses before it writes anything: before the
// `.current` a --remember would write — a choice recorded by a launch that then
// refused would move where a bare launch goes as a side effect of nothing —
// before the placement the store would record, and before exec. Each row
// breaks one thing preparation checks; a dry run refuses it the same way, on
// the same stream.
func TestALaunchThatCannotRunRefusesBeforeWriting(t *testing.T) {
	// A real directory where the shared-sessions link belongs: the
	// history-forking state.
	fork := func(t *testing.T, link string) {
		t.Helper()
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(link, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	forkedClaude := func(t *testing.T) config.Scope {
		cfg := launchConfig(t)
		fork(t, filepath.Join(cfg.AccountsRoot, "yan@planlab.ai", "projects"))
		return cfg
	}
	for _, c := range []struct {
		name  string
		scope func(t *testing.T) config.Scope
		args  []string
		want  []string
	}{
		{"projects is a real directory", forkedClaude,
			[]string{"--remember", "--account", "yan@planlab.ai"}, []string{"not launching"}},
		{"a dry run of it", forkedClaude,
			[]string{"--dry-run", "--account", "yan@planlab.ai"}, []string{"not launching"}},
		// HEADROOM_HOME re-points what headroom observes, but the primary is
		// selected by CLAUDE_CONFIG_DIR being absent — resolved by the vendor
		// against the real home, a tree the board never described.
		{"the primary under a re-pointed home", func(t *testing.T) config.Scope {
			cfg := launchConfig(t)
			cfg.PrimaryRelocated = true
			return cfg
		}, []string{"--account", "qiushi"}, []string{"HEADROOM_HOME"}},
		{"no claude on PATH, over an earlier pin", func(t *testing.T) config.Scope {
			cfg := launchConfig(t)
			if err := accounts.Discover(cfg).SetCurrent(accounts.Account{Scope: cfg, Name: "qiushi"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			return cfg
		}, []string{"--remember", "--account", "yan@planlab.ai"}, []string{"not found on PATH"}},
		{"Codex: sessions is a real directory", func(t *testing.T) config.Scope {
			scope := codexLaunchFixture(t)
			fork(t, filepath.Join(scope.AccountsRoot, "cx@x.com", "sessions"))
			return scope
		}, []string{"--remember", "--account", "cx@x.com"}, []string{"not launching", "sessions"}},
		{"Codex: the primary under a re-pointed home", func(t *testing.T) config.Scope {
			scope := codexLaunchFixture(t)
			scope.PrimaryRelocated = true
			return scope
		}, []string{"--remember", "--account", "primary"}, []string{"HEADROOM_HOME"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.scope(t)
			_, _, _, called := recordExec(t)
			before, _ := os.ReadFile(cfg.CurrentFile())
			var code int
			var stderr string
			stdout := captureStdout(t, func() { stderr = captureStderr(t, func() { code = runLaunch(cfg, c.args) }) })
			if code != 1 || *called {
				t.Fatalf("exit %d, exec called %v: %s", code, *called, stderr)
			}
			for _, w := range c.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr lacks %q: %s", w, stderr)
				}
			}
			if stdout != "" {
				t.Errorf("a refusal printed to stdout:\n%s", stdout)
			}
			if after, _ := os.ReadFile(cfg.CurrentFile()); !bytes.Equal(after, before) {
				t.Errorf(".current %q → %q by a launch that refused", before, after)
			}
			// The store registers, prunes and commits even when its rule
			// refuses: a launch that cannot run must not reach it.
			if _, err := os.Stat(filepath.Join(cfg.AccountsRoot, "state.json")); !os.IsNotExist(err) {
				t.Errorf("a launch that refused wrote state.json (%v)", err)
			}
		})
	}

	// An extra carries its dir explicitly: a re-pointed home leaves it alone.
	cfg := launchConfig(t)
	cfg.PrimaryRelocated = true
	_, _, _, called := recordExec(t)
	if code := runLaunch(cfg, []string{"--account", "yan@planlab.ai"}); code != 0 || !*called {
		t.Errorf("extra under a re-pointed home: exit %d, exec called %v", code, *called)
	}
}

// --remember is written before the exec: read from inside the exec, the
// choice is already recorded, and a failed exec leaves it and says so.
func TestExecFailureKeepsRememberedChoice(t *testing.T) {
	cfg := launchConfig(t)
	prev := execVendor
	atExec := ""
	execVendor = func(string, string, []string, []string) error {
		a, err := accounts.Discover(cfg).Select("")
		atExec = fmt.Sprintf("%s %v", a.Name, err)
		return os.ErrPermission
	}
	t.Cleanup(func() { execVendor = prev })
	output := captureStderr(t, func() {
		if code := runLaunch(cfg, []string{"--remember", "--account", "yan@planlab.ai"}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
	if atExec != "yan@planlab.ai <nil>" {
		t.Errorf("at exec .current resolved to %q, want yan@planlab.ai: the choice is recorded first", atExec)
	}
	if !strings.Contains(output, ".current remains yan@planlab.ai") {
		t.Fatalf("missing persistence explanation: %s", output)
	}

	a, err := accounts.Discover(cfg).Select("")
	if err != nil || a.Name != "yan@planlab.ai" {
		t.Fatalf("choice after failed exec: %+v %v", a, err)
	}
}
