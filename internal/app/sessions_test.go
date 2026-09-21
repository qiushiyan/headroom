package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
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
	execSessions = func(execPath, binary string, claudeArgs, environ []string) error {
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
	ui := &resumeUI{sessionActions: sessionActions{beforeLaunch: func() {},
		cfg:        cfg,
		set:        accounts.Discover(cfg),
		current:    "yan@planlab.ai",
		claudeArgs: []string{"--dangerously-skip-permissions"},
	}}
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
			// Relocation is a fact about the scope, and accounts carry the
			// scope they were discovered under — as a real run's would.
			ui.cfg.PrimaryRelocated = true
			ui.set = accounts.Discover(ui.cfg)
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

func TestSessionPreparationPreservesPriorRehome(t *testing.T) {
	ui, s := sessionsFixture(t)
	ui.st = state.Open(ui.cfg)
	if err := ui.st.ReHome(s.ID, "qiushi", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	done, err := ui.resume(s, true)
	if done || err == nil {
		t.Fatalf("missing executable accepted: %v %v", done, err)
	}
	if owner := ui.st.Load().Owners()[s.ID]; owner.Account != "qiushi" {
		t.Fatalf("preparation failure changed owner: %+v", owner)
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

func TestSessionPathWithoutCDFileAcceptsNewline(t *testing.T) {
	ui, s := sessionsFixture(t)
	_, _, _, called := capturedSessionsExec(t)
	s.CWD = filepath.Join(ui.cfg.Home, "project\nname")
	if err := os.Mkdir(s.CWD, 0755); err != nil {
		t.Fatal(err)
	}
	if done, code := ui.commitResume(false); !done || code != 0 || !*called {
		t.Fatalf("commit: %v %d %s", done, code, ui.message)
	}
}

func TestSessionExecFailureKeepsRehome(t *testing.T) {
	ui, s := sessionsFixture(t)
	capturedSessionsExec(t) // also restores cwd
	ui.st = state.Open(ui.cfg)
	execSessions = func(string, string, []string, []string) error { return os.ErrPermission }
	output := captureStderr(t, func() {
		if done, code := ui.commitResume(true); !done || code != 1 {
			t.Fatalf("commit: %v %d %s", done, code, ui.message)
		}
	})
	if !strings.Contains(output, "re-home remains recorded for "+ui.current) {
		t.Fatalf("missing persistence explanation: %s", output)
	}

	if owner := ui.st.Load().Owners()[s.ID]; owner.Account != ui.current {
		t.Fatalf("re-home after failed exec: %+v", owner)
	}
}

func TestUncertainProcessInspectionPreservesTranscript(t *testing.T) {
	ui, s := sessionsFixture(t)
	registry := filepath.Join(ui.cfg.AccountsRoot, "a", "sessions")
	if err := os.MkdirAll(registry, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "7.json"), []byte(`{"sessionId":"`+s.ID+`","pid":7,"startedAt":1000}`), 0644); err != nil {
		t.Fatal(err)
	}
	ui.refs = []sessions.AccountRef{{Name: "a", Dir: filepath.Dir(registry)}}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\necho unreadable-start-time\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	s.StoreDir = t.TempDir()
	s.Path = filepath.Join(s.StoreDir, s.ID+".jsonl")
	original := []byte("{}\n")
	if err := os.WriteFile(s.Path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ui.rename(s, "changed"); err == nil {
		t.Fatal("uncertain process allowed rename")
	}
	if removed, err := ui.delete(s); removed || err == nil {
		t.Fatal("uncertain process allowed delete")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("transcript changed: %q %v", data, err)
	}
}
