package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// capturedSessionsExec swaps the sessions exec edge for a recorder and pins
// the test's working directory back afterwards — commitResume chdirs.
func capturedSessionsExec(t *testing.T) (path *string, argv *[]string, env *[]string, called *bool) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	var p string
	var a, e []string
	var c bool
	prev := execSessions
	execSessions = func(execPath string, claudeArgs, environ []string) error {
		p, a, e, c = execPath, claudeArgs, environ, true
		return nil
	}
	t.Cleanup(func() { execSessions = prev })
	return &p, &a, &e, &c
}

func sessionsFixture(t *testing.T) (*resumeUI, *sessions.Session) {
	t.Helper()
	cfg := launchConfig(t) // valid topology for yan@planlab.ai
	proj := filepath.Join(cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	ui := &resumeUI{
		cfg:        cfg,
		accts:      accounts.Discover(cfg),
		current:    "yan@planlab.ai",
		claudeArgs: []string{"--dangerously-skip-permissions"},
	}
	s := &sessions.Session{
		ID: "11111111-2222-3333-4444-555555555555", CWD: proj, DirOK: true,
		Owner: "yan@planlab.ai", OwnerState: sessions.OwnerHistory,
	}
	ui.listing.Sessions = []*sessions.Session{s}
	ui.rows = ui.listing.Sessions
	return ui, s
}

// The commit becomes the session: chdir to the verified cwd, personal args
// first and the picker's own --resume last, the owner's dir set exactly
// once, PWD corrected to the entered dir, and the advisory cd file carrying
// the dir — all decided in-process, nothing crossing a protocol.
func TestSessionsCommitExecsOnOwner(t *testing.T) {
	ui, s := sessionsFixture(t)
	_, argv, env, called := capturedSessionsExec(t)

	cd := filepath.Join(t.TempDir(), "cwd")
	if f, err := os.Create(cd); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	ui.cdFile = cd

	done, code := ui.commitResume(false)
	if !done || code != 0 || !*called {
		t.Fatalf("commit = (%v,%d), exec called %v", done, code, *called)
	}
	want := []string{"--dangerously-skip-permissions", "--resume", s.ID}
	if strings.Join(*argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", *argv, want)
	}
	wantDir := "CLAUDE_CONFIG_DIR=" + filepath.Join(ui.cfg.AccountsRoot, "yan@planlab.ai")
	nDir, pwd := 0, ""
	for _, kv := range *env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			nDir++
			if kv != wantDir {
				t.Errorf("env %q, want %q", kv, wantDir)
			}
		}
		if v, ok := strings.CutPrefix(kv, "PWD="); ok {
			pwd = v
		}
	}
	if nDir != 1 {
		t.Errorf("%d CLAUDE_CONFIG_DIR entries, want 1", nDir)
	}
	if pwd != s.CWD {
		t.Errorf("PWD = %q, want the entered dir %q", pwd, s.CWD)
	}
	// Inode identity, not string equality: Getwd resolves symlinks (macOS
	// /var → /private/var) while s.CWD keeps the recorded spelling.
	here, err1 := os.Stat(".")
	want2, err2 := os.Stat(s.CWD)
	if err1 != nil || err2 != nil || !os.SameFile(here, want2) {
		wd, _ := os.Getwd()
		t.Errorf("cwd = %q, want %q", wd, s.CWD)
	}
	if b, err := os.ReadFile(cd); err != nil || string(b) != s.CWD {
		t.Errorf("cd file = (%q, %v), want the entered dir", b, err)
	}
}

// Every refusal returns to the picker with a message, execs nothing, and
// leaves the advisory file empty — empty is the wrapper's "do not cd".
func TestSessionsCommitRefusalsExecNothing(t *testing.T) {
	cases := []struct {
		name  string
		wreck func(ui *resumeUI, s *sessions.Session)
		want  string
	}{
		{"broken topology", func(ui *resumeUI, s *sessions.Session) {
			link := filepath.Join(ui.cfg.AccountsRoot, "yan@planlab.ai", "projects")
			os.Remove(link)
			os.MkdirAll(link, 0o755) // real dir: the history fork
		}, "not launching"},
		{"relocated primary", func(ui *resumeUI, s *sessions.Session) {
			ui.cfg.PrimaryRelocated = true
			s.Owner = "qiushi"
		}, "HEADROOM_HOME"},
		{"dir gone at action time", func(ui *resumeUI, s *sessions.Session) {
			os.RemoveAll(s.CWD)
		}, "directory is gone"},
		{"no account at all", func(ui *resumeUI, s *sessions.Session) {
			s.Owner, ui.current = "", ""
		}, "no account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui, s := sessionsFixture(t)
			_, _, _, called := capturedSessionsExec(t)
			cd := filepath.Join(t.TempDir(), "cwd")
			if f, err := os.Create(cd); err != nil {
				t.Fatal(err)
			} else {
				f.Close()
			}
			ui.cdFile = cd
			c.wreck(ui, s)

			done, code := ui.commitResume(false)
			if done || code != 0 {
				t.Errorf("refusal ended the picker: (%v,%d)", done, code)
			}
			if *called {
				t.Error("exec ran through a refusal")
			}
			if !strings.Contains(ui.message, c.want) {
				t.Errorf("message %q does not name the refusal (%q)", ui.message, c.want)
			}
			if b, _ := os.ReadFile(cd); len(b) != 0 {
				t.Errorf("cd file non-empty (%q) after a refusal — the wrapper would cd", b)
			}
		})
	}
}

// The surface's own contract refusals, before any terminal is touched.
func TestSessionsArgContract(t *testing.T) {
	cfg := launchConfig(t)
	if code := runSessions(cfg, []string{"--", "--resume", "x"}); code != 2 {
		t.Errorf("--resume pass-through: exit %d, want 2 (the picker chooses the session)", code)
	}
	if code := runSessions(cfg, []string{"--cd-file", "relative/path"}); code != 2 {
		t.Errorf("relative --cd-file: exit %d, want 2", code)
	}
	// Under go test stdin is not a terminal: the TTY guard must refuse before
	// a picker that would exec claude onto a pipe.
	if code := runSessions(cfg, nil); code != 1 {
		t.Errorf("no terminal: exit %d, want 1", code)
	}
}
