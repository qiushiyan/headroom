package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// keepWorkingDir puts the test's working directory back afterwards: a
// committed resume chdirs into the session's project before it execs, as the
// real one does. The exec itself is recorded (execRecord).
func keepWorkingDir(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

func sessionsFixture(t *testing.T) (*resumeUI, *sessions.Session) {
	t.Helper()
	cfg := launchConfig(t) // valid topology for yan@planlab.ai
	proj := filepath.Join(cfg.Home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	set := accounts.Discover(cfg)
	// The real process table, through the ps(1) a test puts on PATH, and no
	// refresh left behind.
	place := LivePlace()
	place.Refresh = func(config.Scope) error { return nil }
	ui := &resumeUI{beforeLaunch: func() {},
		st:         state.Open(cfg),
		set:        set,
		claudeArgs: []string{"--dangerously-skip-permissions"},
		place:      place, exec: (&execRecord{}).exec, errw: &bytes.Buffer{}}
	s := &sessions.Session{
		ID: "11111111-2222-3333-4444-555555555555", CWD: proj, DirOK: true,
		Owner: "yan@planlab.ai", OwnerState: sessions.OwnerHistory,
	}
	ui.listing.Sessions = []*sessions.Session{s}
	ui.rows = ui.listing.Sessions
	return ui, s
}

// stubPS puts a ps(1) running script first on PATH: the picker samples
// processes through the real ps, and this is what it answers.
func stubPS(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// claimSession writes the vendor's live-session record for id into an
// account's registry, as a running claude does.
func claimSession(t *testing.T, accountDir, id string, pid int, started time.Time) {
	t.Helper()
	dir := filepath.Join(accountDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, fmt.Sprintf("%d.json", pid)),
		fmt.Sprintf(`{"sessionId":%q,"pid":%d,"startedAt":%d}`, id, pid, started.UnixMilli()))
}

// The commit becomes the session: chdir to the verified cwd, personal args
// first and the picker's own --resume last, the owner's dir set exactly
// once, PWD corrected to the entered dir, and the advisory cd file carrying
// the dir — all decided in-process, nothing crossing a protocol.
func TestSessionsCommitExecsOnOwner(t *testing.T) {
	ui, s := sessionsFixture(t)
	keepWorkingDir(t)
	rec := rearm(ui)

	cd := filepath.Join(t.TempDir(), "cwd")
	if f, err := os.Create(cd); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	ui.cdFile = cd

	done, code := ui.commitResume(false)
	if !done || code != 0 || !rec.called {
		t.Fatalf("commit = (%v,%d), exec called %v", done, code, rec.called)
	}
	want := []string{"--dangerously-skip-permissions", "--resume", s.ID}
	if strings.Join(rec.args, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", rec.args, want)
	}
	wantDir := "CLAUDE_CONFIG_DIR=" + filepath.Join(ui.set.Scope.AccountsRoot, "yan@planlab.ai")
	nDir, pwd := 0, ""
	for _, kv := range rec.env {
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
		wreck func(t *testing.T, ui *resumeUI, s *sessions.Session)
		want  string
	}{
		{"live elsewhere", func(t *testing.T, ui *resumeUI, s *sessions.Session) {
			// Open in another terminal on its owner: a second writer on one
			// transcript. The claim's pid runs and started when it says.
			started := time.Now().Add(-time.Minute).Truncate(time.Second)
			claimSession(t, filepath.Join(ui.set.Scope.AccountsRoot, "yan@planlab.ai"), s.ID, 4242, started)
			stubPS(t, "echo '"+started.Format("Mon Jan _2 15:04:05 2006")+"'")
		}, "another terminal"},
		{"newline in the cwd, with a cd file", func(t *testing.T, ui *resumeUI, s *sessions.Session) {
			// The cd file is one line; this dir cannot be written to it.
			s.CWD = filepath.Join(ui.set.Scope.Home, "project\nname")
			if err := os.Mkdir(s.CWD, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "newline"},
		{"broken topology", func(t *testing.T, ui *resumeUI, s *sessions.Session) {
			link := filepath.Join(ui.set.Scope.AccountsRoot, "yan@planlab.ai", "projects")
			os.Remove(link)
			os.MkdirAll(link, 0o755) // real dir: the history fork
		}, "not launching"},
		{"relocated primary", func(t *testing.T, ui *resumeUI, s *sessions.Session) {
			// Relocation is a fact about the scope, and accounts carry the
			// scope they were discovered under — as a real run's would.
			cfg := ui.set.Scope
			cfg.PrimaryRelocated = true
			ui.set = accounts.Discover(cfg)
			s.Owner = "qiushi"
		}, "HEADROOM_HOME"},
		{"dir gone at action time", func(t *testing.T, ui *resumeUI, s *sessions.Session) {
			os.RemoveAll(s.CWD)
		}, "directory is gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui, s := sessionsFixture(t)
			keepWorkingDir(t)
			rec := rearm(ui)
			cd := filepath.Join(t.TempDir(), "cwd")
			if f, err := os.Create(cd); err != nil {
				t.Fatal(err)
			} else {
				f.Close()
			}
			ui.cdFile = cd
			c.wreck(t, ui, s)

			done, code := ui.commitResume(false)
			if done || code != 0 {
				t.Errorf("refusal ended the picker: (%v,%d)", done, code)
			}
			if rec.called {
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
	ui.st = state.Open(ui.set.Scope)
	if err := rehome(ui.st, s.ID, "qiushi", time.Now()); err != nil {
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

// The listing routes by the re-homes the store holds: collectSessions must hand
// them to the collector, or a session the user moved with x lists — and
// resumes — on the account its history names instead.
func TestCollectSessionsRoutesByTheStoredReHomes(t *testing.T) {
	cfg := launchConfig(t)
	proj := filepath.Join(cfg.Home, "proj")
	store := filepath.Join(cfg.StoreDir(), sessions.Munge(proj))
	for _, dir := range []string{proj, store} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const id = "11111111-2222-3333-4444-555555555555"
	writeJSON(t, filepath.Join(store, id+".jsonl"), fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q}`+"\n", id, proj))
	st := state.Open(cfg)
	if err := rehome(st, id, "yan@planlab.ai", time.Now()); err != nil {
		t.Fatal(err)
	}

	listing, _ := collectSessions(cfg, st, quietPlace().Probe)
	if len(listing.Sessions) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(listing.Sessions))
	}
	if s := listing.Sessions[0]; s.Owner != "yan@planlab.ai" || s.OwnerState != sessions.OwnerRehome {
		t.Errorf("owner (%q, %v), want the re-home's account", s.Owner, s.OwnerState)
	}
}

// The surface's own contract refusals, before any terminal is touched.
func TestSessionsArgContract(t *testing.T) {
	cfg := launchConfig(t)
	run := func(opts SessionsOptions) int {
		return call(func(std Streams) error {
			return Sessions(std, Terminal{}, cfg, opts, SessionsDeps{Place: quietPlace(), Exec: (&execRecord{}).exec})
		}).code
	}
	if code := run(SessionsOptions{Args: []string{"--resume", "x"}}); code != 2 {
		t.Errorf("--resume pass-through: exit %d, want 2 (the picker chooses the session)", code)
	}
	if code := run(SessionsOptions{CDFile: "relative/path"}); code != 2 {
		t.Errorf("relative --cd-file: exit %d, want 2", code)
	}
	// Off a terminal the TTY guard must refuse before a picker that would
	// exec claude onto a pipe.
	if code := run(SessionsOptions{}); code != 1 {
		t.Errorf("no terminal: exit %d, want 1", code)
	}
}

func TestSessionPathWithoutCDFileAcceptsNewline(t *testing.T) {
	ui, s := sessionsFixture(t)
	keepWorkingDir(t)
	rec := rearm(ui)
	s.CWD = filepath.Join(ui.set.Scope.Home, "project\nname")
	if err := os.Mkdir(s.CWD, 0755); err != nil {
		t.Fatal(err)
	}
	if done, code := ui.commitResume(false); !done || code != 0 || !rec.called {
		t.Fatalf("commit: %v %d %s", done, code, ui.message)
	}
}

func TestSessionExecFailureKeepsRehome(t *testing.T) {
	ui, s := sessionsFixture(t)
	keepWorkingDir(t)
	ui.st = state.Open(ui.set.Scope)
	ui.exec = (&execRecord{err: os.ErrPermission}).exec
	if done, code := ui.commitResume(true); !done || code != 1 {
		t.Fatalf("commit: %v %d %s", done, code, ui.message)
	}
	owner := ui.st.Load().Owners()[s.ID]
	if owner.Account == "" || owner.Account == "yan@planlab.ai" {
		t.Fatalf("re-home after failed exec: %+v", owner)
	}
	if output := ui.failed.Error(); !strings.Contains(output, "re-home remains recorded for "+ui.set.NameOf(owner.Account)) {
		t.Fatalf("missing persistence explanation: %s", output)
	}
}

// claimedTranscript is a session with a transcript on disk whose owner's
// registry claims it under pid 7: what ps then says about pid 7 decides
// whether r and dd may touch it.
func claimedTranscript(t *testing.T) (*resumeUI, *sessions.Session, []byte) {
	t.Helper()
	ui, s := sessionsFixture(t)
	claimSession(t, filepath.Join(ui.set.Scope.AccountsRoot, "yan@planlab.ai"), s.ID, 7, time.UnixMilli(1000))
	s.StoreDir = t.TempDir()
	s.Path = filepath.Join(s.StoreDir, s.ID+".jsonl")
	original := []byte(`{"type":"user","sessionId":"` + s.ID + `","message":{"role":"user","content":"keep me"}}` + "\n")
	if err := os.WriteFile(s.Path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	return ui, s, original
}

// Whatever ps answers short of "no such process" leaves the claim unproven,
// and r and dd refuse an unproven claim: a ps that fails outright — a sandbox
// denying it, say — must never read as the session being closed.
func TestUncertainProcessInspectionPreservesTranscript(t *testing.T) {
	for _, c := range []struct{ name, ps string }{
		{"a start time ps cannot render", "echo unreadable-start-time"},
		{"ps failing with a reason", "echo 'ps: operation not permitted' >&2; exit 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ui, s, original := claimedTranscript(t)
			stubPS(t, c.ps)
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
		})
	}
}

// ps exiting 1 with nothing on either stream is its answer for "no such
// process": the claim is stale, the session is not open, and r goes ahead.
// Read as a failed inspection instead, every stale registry file would refuse
// r, dd and accounts remove for good. The rename is one appended record, as
// the vendor's own: the transcript it was given is left byte for byte.
func TestAbsentProcessLeavesTheSessionMutable(t *testing.T) {
	ui, s, original := claimedTranscript(t)
	stubPS(t, "exit 1")
	if err := ui.rename(s, "changed"); err != nil {
		t.Fatalf("a claim whose process is gone refused rename: %v", err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	added, kept := bytes.CutPrefix(data, original)
	if !kept || bytes.Count(added, []byte("\n")) != 1 {
		t.Fatalf("rename rewrote the transcript instead of appending one record: %q", data)
	}
	if got := sessions.ParseTail(data, "", true).CustomTitle; got != "changed" {
		t.Errorf("transcript's custom title = %q, want the rename", got)
	}
}
