package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

func launchConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, "yan@planlab.ai"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// capturedExec swaps the exec edge for a recorder. Restore is automatic.
func capturedExec(t *testing.T) (args *[]string, env *[]string, called *bool) {
	t.Helper()
	var a, e []string
	var c bool
	prev := execClaude
	execClaude = func(claudeArgs, environ []string) error {
		a, e, c = claudeArgs, environ, true
		args, env = &a, &e
		return nil
	}
	t.Cleanup(func() { execClaude = prev })
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

func TestLaunchRememberRecordsBeforeExec(t *testing.T) {
	cfg := launchConfig(t)
	_, _, called := capturedExec(t)

	if code := runLaunch(cfg, []string{"--remember", "--account", "yan@planlab.ai"}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !*called {
		t.Fatal("exec never ran")
	}
	if a, err := accounts.Select(cfg, accounts.Discover(cfg), ""); err != nil || a.Name != "yan@planlab.ai" {
		t.Errorf(".current = (%q, %v), want yan@planlab.ai", a.Name, err)
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

func TestLaunchRefusesExplicitlyEmptyAccount(t *testing.T) {
	cfg := launchConfig(t)
	_, _, called := capturedExec(t)

	// An explicitly empty --account is a malformed name, not "the recorded
	// choice": collapsing the two lets a caller's expansion bug launch the
	// recorded account instead of refusing. Pinned red against the pre-fix
	// binary, which launched the primary here.
	if code := runLaunch(cfg, []string{"--account", ""}); code != 2 {
		t.Errorf("--account \"\": exit %d, want 2", code)
	}
	if *called {
		t.Error("exec ran on an explicitly empty account name")
	}
}
