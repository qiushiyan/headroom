package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
// exactly one for an extra. Arguments after `--` reach the vendor unchanged. A
// named launch that names a session records nothing about it: it was told
// where to go, and a re-home written onto the name would steer the session's
// next automatic turn.
func TestALaunchNamesExactlyItsAccount(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		dirs func(cfg config.Scope) []string
	}{
		{"the primary, named", []string{"--account", "qiushi", "--", "--resume", sessA},
			func(config.Scope) []string { return nil }},
		{"an extra, named", []string{"--account", "yan@planlab.ai", "--", "--resume", sessA},
			func(cfg config.Scope) []string { return []string{filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := launchConfig(t)
			t.Setenv("CLAUDE_CONFIG_DIR", "/leaked/other-account")
			got := launchOn(t, cfg, quietPlace(), c.args...)
			if got.code != 0 || !got.called {
				t.Fatalf("exit %d, exec called %v: %s", got.code, got.called, got.stderr)
			}
			if got, want := configDirs(got.env), c.dirs(cfg); !slices.Equal(got, want) {
				t.Errorf("CLAUDE_CONFIG_DIR = %q, want exactly %q", got, want)
			}
			var rest []string
			if i := slices.Index(c.args, "--"); i >= 0 {
				rest = c.args[i+1:]
			}
			if !slices.Equal(got.args, rest) {
				t.Errorf("vendor args = %q, want %q", got.args, rest)
			}
			if rec, ok := state.Open(cfg).Load().Owners()[sessA]; ok {
				t.Errorf("a launch told where to go re-homed the session it named: %+v", rec)
			}
		})
	}
}

// A launch that cannot run refuses before it writes anything: before the
// placement the store would record, and before exec. Each row
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
			[]string{"--account", "yan@planlab.ai"}, []string{"not launching"}},
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
		{"no claude on PATH", func(t *testing.T) config.Scope {
			cfg := launchConfig(t)
			t.Setenv("PATH", t.TempDir())
			return cfg
		}, []string{"--account", "yan@planlab.ai"}, []string{"not found on PATH"}},
		{"Codex: sessions is a real directory", func(t *testing.T) config.Scope {
			scope := codexLaunchFixture(t)
			fork(t, filepath.Join(scope.AccountsRoot, "cx@x.com", "sessions"))
			return scope
		}, []string{"--account", "cx@x.com"}, []string{"not launching", "sessions"}},
		{"Codex: the primary under a re-pointed home", func(t *testing.T) config.Scope {
			scope := codexLaunchFixture(t)
			scope.PrimaryRelocated = true
			return scope
		}, []string{"--account", "primary"}, []string{"HEADROOM_HOME"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.scope(t)
			got := launchOn(t, cfg, quietPlace(), c.args...)
			stdout, stderr := got.stdout, got.stderr
			if got.code != 1 || got.called {
				t.Fatalf("exit %d, exec called %v: %s", got.code, got.called, stderr)
			}
			for _, w := range c.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr lacks %q: %s", w, stderr)
				}
			}
			if stdout != "" {
				t.Errorf("a refusal printed to stdout:\n%s", stdout)
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
	if got := launchOn(t, cfg, quietPlace(), "--account", "yan@planlab.ai"); got.code != 0 || !got.called {
		t.Errorf("extra under a re-pointed home: exit %d, exec called %v", got.code, got.called)
	}
}

// A failed exec is the launch's failure, said with the vendor's name.
func TestAFailedExecSaysSo(t *testing.T) {
	cfg := launchConfig(t)
	rec := &execRecord{err: os.ErrPermission}
	failed := call(func(std Streams) error {
		return Launch(std, cfg, launchOptions(t, "--account", "yan@planlab.ai"), quietPlace(), rec.exec)
	})
	if failed.code != 1 || !strings.Contains(failed.stderr, "exec claude: permission denied") {
		t.Fatalf("exit %d: %s", failed.code, failed.stderr)
	}
}

// configDirs is every CLAUDE_CONFIG_DIR a child environment carries.
func configDirs(env []string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			out = append(out, v)
		}
	}
	return out
}
