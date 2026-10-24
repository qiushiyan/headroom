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

// capturedExec swaps the exec edge for a recorder. Restore is automatic.
func capturedExec(t *testing.T) (args *[]string, env *[]string, called *bool) {
	t.Helper()
	var a, e []string
	var c bool
	prev := execVendor
	execVendor = func(path, binary string, claudeArgs, environ []string) error {
		a, e, c = claudeArgs, environ, true
		args, env = &a, &e
		return nil
	}
	t.Cleanup(func() { execVendor = prev })
	return &a, &e, &c
}

func TestLaunchPrimaryStripsInheritedConfigDir(t *testing.T) {
	cfg := launchConfig(t)
	args, env, called := capturedExec(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/leaked/other-account")

	if code := runLaunch(cfg, []string{"--", "--resume", "abc"}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !*called {
		t.Fatal("exec never ran")
	}
	for _, kv := range *env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("primary launch leaked %q", kv)
		}
	}
	if !slices.Equal(*args, []string{"--resume", "abc"}) {
		t.Errorf("claude args = %v", *args)
	}
}

func TestLaunchExtraAccountSetsExactlyItsDir(t *testing.T) {
	cfg := launchConfig(t)
	_, env, _ := capturedExec(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/leaked/other-account")

	if code := runLaunch(cfg, []string{"--account", "yan@planlab.ai"}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	want := "CLAUDE_CONFIG_DIR=" + filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	n := 0
	for _, kv := range *env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			n++
			if kv != want {
				t.Errorf("got %q, want %q", kv, want)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d CLAUDE_CONFIG_DIR entries, want exactly 1", n)
	}
}

func TestLaunchRefusesCorruptOrUnknownSelection(t *testing.T) {
	cfg := launchConfig(t)
	_, _, called := capturedExec(t)

	// Empty .current: fail closed, no primary fallback, no exec.
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.CurrentFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runLaunch(cfg, nil); code != 1 {
		t.Errorf("empty .current: exit %d, want 1", code)
	}

	// Unknown explicit account: same refusal.
	if code := runLaunch(cfg, []string{"--account", "gone@x.com"}); code != 1 {
		t.Errorf("unknown account: exit %d, want 1", code)
	}
	if *called {
		t.Error("exec ran despite refused selection")
	}
}

func TestResolvePrintsNameAndRealDir(t *testing.T) {
	cfg := launchConfig(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	code := runResolve(cfg, nil) // no .current → primary
	w.Close()
	os.Stdout = prev
	out := make([]byte, 256)
	n, _ := r.Read(out)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	// The primary resolves to its real dir, never an empty sentinel, and the
	// kind field is what the wrapper's preflight keys on — re-deriving
	// primary-vs-extra from path prefixes is exactly what a HEADROOM_* root
	// override silently breaks.
	want := "qiushi\t" + cfg.PrimaryDir() + "\tprimary\n"
	if string(out[:n]) != want {
		t.Errorf("resolve output %q, want %q", out[:n], want)
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

// A primary spelled out by its dir names the tree the board describes, so a
// re-pointed home does not refuse it: the refusal exists for selection by
// absence, which the vendor resolves against the real home.
func TestASpelledOutPrimaryLaunchesByItsDir(t *testing.T) {
	cfg := launchConfig(t)
	cfg.PrimaryExplicit, cfg.PrimaryRelocated = true, true
	_, env, called := capturedExec(t)
	if code := runLaunch(cfg, []string{"--account", "qiushi"}); code != 0 || !*called {
		t.Fatalf("spelled-out primary: exit %d, exec %v", code, *called)
	}
	var dirs []string
	for _, kv := range *env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			dirs = append(dirs, v)
		}
	}
	if len(dirs) != 1 || dirs[0] != cfg.PrimaryDir() {
		t.Errorf("CLAUDE_CONFIG_DIR = %v, want exactly the primary's dir", dirs)
	}
}
