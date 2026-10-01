package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// sessionActions owns routing and launch effects. The picker supplies terminal
// restoration as the commit boundary; action tests need no terminal object.
type sessionActions struct {
	cfg     config.Scope
	st      *state.Store
	refs    []sessions.AccountRef
	set     accounts.Set
	current string
	// auto says bare launches choose for themselves: there is then no current
	// account, and wherever this surface would fall back to one it asks the
	// placement rule instead.
	auto         bool
	cdFile       string
	claudeArgs   []string
	beforeLaunch func()
}

func (actions *sessionActions) resume(s *sessions.Session, override bool) (bool, error) {
	if actions.liveNow(s) == sessions.Live {
		return false, fmt.Errorf("open in another terminal — switch there instead")
	}
	if !s.DirOK {
		return false, fmt.Errorf("project directory is gone — dd deletes the session")
	}
	if actions.cdFile != "" && strings.Contains(s.CWD, "\n") {
		return false, fmt.Errorf("project path contains a newline — not representable in the cd file")
	}
	if fi, err := os.Stat(s.CWD); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("project directory is gone — dd deletes the session")
	}
	intent, ok := actions.resumeIntent(s, override)
	if !ok {
		return false, fmt.Errorf("no account to resume on — run headroom accounts")
	}

	// From here the resume is a launch like any other: one operation decides
	// the account, records the launch as load and — for the override — the
	// re-home with it, and logs it. A refusal there has recorded nothing.
	now := time.Now()
	req := launchRequest{st: actions.st, intent: intent, mode: "picker", ref: sessionRef{Route: s.ID}, owner: s.Owner, cwd: s.CWD, now: now}
	if override {
		// The override moves the session, so its routing is part of the
		// launch: written with it, or the launch does not happen.
		req.ref.Record, req.mustReHome = s.ID, true
		req.live = func() (map[string]bool, bool) { return sessions.TranscriptIDs(actions.cfg.StoreDir()) }
	}
	facts := gatherPlacement(actions.set, actions.st, os.Environ(), intent.Kind == placement.Auto, now)
	out, err := placeLaunch(req, facts)
	if err != nil {
		return false, err
	}

	actions.beforeLaunch()
	out.announce(os.Stderr, "headroom sessions: ")
	recorded := ""
	if override {
		recorded = "; re-home remains recorded for " + out.account.Name
	}
	if err := os.Chdir(s.CWD); err != nil {
		return true, fmt.Errorf("cd %s: %v%s", s.CWD, err, recorded)
	}
	if actions.cdFile != "" {
		if err := writeCDFile(actions.cdFile, s.CWD); err != nil {
			fmt.Fprintf(os.Stderr, "headroom sessions: cd file: %v (continuing)\n", err)
		}
	}
	argv := append(append([]string{}, actions.claudeArgs...), "--resume", s.ID)
	if err := execSessions(out.prepared.Path, out.prepared.Binary, argv, envWithPWD(out.prepared.Env, s.CWD)); err != nil {
		return true, fmt.Errorf("exec claude: %v%s", err, recorded)
	}
	return true, nil
}

// resumeIntent says how a resume's account is decided: the owner, then valid
// current. An override chooses current directly; an unresolved current leaves
// the action without a target, never the primary, which no evidence chose.
//
// Under automatic placement there is no current account, and each place this
// would have fallen back to one asks the rule instead: a session with no
// evidence, or whose owner is gone, is placed like a new one, and the override
// moves the session to the least-loaded of the *other* accounts — moving means
// somewhere other than where it is.
func (actions *sessionActions) resumeIntent(s *sessions.Session, override bool) (placement.Intent, bool) {
	known := func(name string) bool {
		for _, a := range actions.set.Accounts {
			if a.Name == name {
				return name != ""
			}
		}
		return false
	}
	switch {
	case !override && known(s.Owner):
		return placement.Intent{Kind: placement.Forced, Account: s.Owner, Reason: placement.ReasonOwner}, true
	case actions.auto && override:
		return placement.Intent{Exclude: s.Owner}, true
	case actions.auto:
		return placement.Intent{}, true
	case known(actions.current):
		// Degraded attribution — no evidence, or an owner the filesystem no
		// longer has — falls back to the *current* account; the row's owner
		// tag already says so.
		return placement.Intent{Kind: placement.Forced, Account: actions.current, Reason: "pinned"}, true
	}
	return placement.Intent{}, false
}

// liveNow re-establishes the selected session's liveness at action time —
// the listing's snapshot is display state, and a session opened since the
// picker drew must still gate every mutation and the resume refusal. The
// fresh answer also lands back on the row so the display tells the truth.
func (actions *sessionActions) liveNow(s *sessions.Session) sessions.LiveState {
	s.Live = sessions.LiveNow(s.ID, actions.refs, psProbe)
	return s.Live
}

// envWithPWD replaces the inherited PWD with the entered dir: os.Chdir moves
// the kernel cwd but not the environment, and the shell-set PWD would
// otherwise describe the directory the picker was invoked from.
func envWithPWD(env []string, cwd string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}
	return append(out, "PWD="+cwd)
}

// writeCDFile publishes the entered dir to the advisory file created at flag
// parse. Truncate-then-write on the already-created path; a Sync so the line
// survives the process being replaced moments later.
func writeCDFile(path, cwd string) error {
	f, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cwd); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// A fresh registry sample gates each vendor mutation, including the commit
// after the user has spent time editing a title or confirming deletion.
func (actions *sessionActions) rename(s *sessions.Session, title string) error {
	if actions.liveNow(s) != sessions.NotLive {
		return fmt.Errorf("session is open elsewhere — rename there (Ctrl+R)")
	}
	if err := sessions.AppendCustomTitle(s.Path, s.ID, title); err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}
	return nil
}

func (actions *sessionActions) delete(s *sessions.Session) (bool, error) {
	if actions.liveNow(s) != sessions.NotLive {
		return false, fmt.Errorf("session is open elsewhere — not deleting")
	}
	if err := sessions.DeleteTranscript(s); err != nil {
		return false, fmt.Errorf("delete failed: %w", err)
	}
	if err := actions.st.Forget(s.ID); err != nil {
		return true, fmt.Errorf("re-home cleanup failed: %w", err)
	}
	return true, nil
}
