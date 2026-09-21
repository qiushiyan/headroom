package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A machine without Codex: every command naming it fails with that reason —
// except accounts add, which is how the first Codex directory comes to exist.
func TestCommandsOnAnAbsentVendor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	for _, v := range []string{"HEADROOM_ACCOUNTS_ROOT", "HEADROOM_CODEX_ACCOUNTS_ROOT", "HEADROOM_PRIMARY_NAME", "HEADROOM_CODEX_PRIMARY_NAME",
		"HEADROOM_LAUNCHER_FORMAT", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL", "HEADROOM_CODEX_USAGE_URL"} {
		t.Setenv(v, "")
	}
	t.Setenv("PATH", t.TempDir())
	// No fixture account can authorize a request, and neither URL is live
	// should one ever try.
	t.Setenv("HEADROOM_USAGE_URL", "http://127.0.0.1:1/usage")
	t.Setenv("HEADROOM_CODEX_USAGE_URL", "http://127.0.0.1:1/usage")

	for _, args := range [][]string{
		{"launch", "--vendor", "codex", "--account", "a@x.com"},
		{"resolve", "--vendor", "codex"},
		{"limits", "--vendor", "codex"},
		{"accounts", "remove", "--vendor", "codex", "a@x.com"},
		{"accounts", "--vendor", "codex"},
		{"--json", "--vendor", "codex"},
	} {
		stderr := captureStderr(t, func() {
			if code := Run(args); code != 1 {
				t.Errorf("%v: exit %d, want 1", args, code)
			}
		})
		if !strings.Contains(stderr, "Codex was not found on this machine") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex-accounts")); err == nil {
		t.Fatal("a refused command created the Codex accounts root")
	}

	for _, args := range [][]string{{"launch", "--vendor", "gemini"}, {"limits", "--vendor"}} {
		stderr := captureStderr(t, func() {
			if code := Run(args); code != 2 {
				t.Errorf("%v: exit %d, want 2", args, code)
			}
		})
		if !strings.Contains(stderr, "vendor") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}

	// accounts add bootstraps the tree, and Codex is present afterwards.
	stdout := captureStdout(t, func() {
		if code := Run([]string{"accounts", "add", "--vendor", "codex", "new@x.com"}); code != 0 {
			t.Fatalf("accounts add --vendor codex: exit %d", code)
		}
	})
	if !strings.Contains(stdout, "seeded "+filepath.Join(home, ".codex-accounts", "new@x.com")) {
		t.Errorf("stdout = %q", stdout)
	}
	if target, err := os.Readlink(filepath.Join(home, ".codex-accounts", "new@x.com", "sessions")); err != nil || target != filepath.Join(home, ".codex", "sessions") {
		t.Errorf("sessions link = %q, %v", target, err)
	}
	out := captureStdout(t, func() {
		if code := Run([]string{"resolve", "--vendor", "codex", "new@x.com"}); code != 0 {
			t.Errorf("resolve after add: exit %d", code)
		}
	})
	if out != "new@x.com\t"+filepath.Join(home, ".codex-accounts", "new@x.com")+"\textra\n" {
		t.Errorf("resolve = %q", out)
	}
	// The reporting surfaces, through the public command line: unfiltered they
	// hold both vendors, and --vendor restricts accounts and current to one.
	var all, only doc5
	if err := json.Unmarshal([]byte(captureStdout(t, func() { Run([]string{"limits"}) })), &all); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(captureStdout(t, func() { Run([]string{"limits", "--vendor", "codex"}) })), &only); err != nil {
		t.Fatal(err)
	}
	if len(all.Current) != 2 || len(only.Current) != 1 || len(only.Accounts) != 2 {
		t.Errorf("limits: unfiltered current %v; filtered current %v, %d accounts", all.Current, only.Current, len(only.Accounts))
	}
	for _, a := range only.Accounts {
		if a.Vendor != "codex" {
			t.Errorf("limits --vendor codex returned a %s account", a.Vendor)
		}
	}
	// The fetching surface filters the same way.
	var fetched doc5
	if err := json.Unmarshal([]byte(captureStdout(t, func() { Run([]string{"--json", "--vendor", "codex"}) })), &fetched); err != nil {
		t.Fatal(err)
	}
	if len(fetched.Current) != 1 || len(fetched.Accounts) != 2 || fetched.Accounts[0].Vendor != "codex" {
		t.Errorf("--json --vendor codex: current %v, accounts %+v", fetched.Current, fetched.Accounts)
	}

	// And a launch: --vendor reaches the Codex scope, the binary and argv[0]
	// are Codex's, and the child's own arguments pass through untouched.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	binary, args, env, called := recordExec(t)
	captureStderr(t, func() {
		if code := Run([]string{"launch", "--vendor", "codex", "--account", "new@x.com", "--", "exec", "--vendor", "x"}); code != 0 {
			t.Errorf("launch --vendor codex: exit %d", code)
		}
	})
	if !*called || *binary != "codex" || strings.Join(*args, " ") != "exec --vendor x" {
		t.Errorf("launch: called=%v argv0=%q args=%v", *called, *binary, *args)
	}
	if v, _ := envValue(*env, "CODEX_HOME"); v != filepath.Join(home, ".codex-accounts", "new@x.com") {
		t.Errorf("CODEX_HOME = %q", v)
	}

	// Every existing invocation means what it meant: no --vendor is claude.
	out = captureStdout(t, func() { Run([]string{"resolve"}) })
	if !strings.HasSuffix(out, filepath.Join(home, ".claude")+"\tprimary\n") {
		t.Errorf("bare resolve = %q", out)
	}
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	defer func() { os.Stdout = previous; f.Close() }()
	os.Stdout = f
	run()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
