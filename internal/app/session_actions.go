package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// sessionActions owns routing and launch effects. The picker supplies terminal
// restoration as the commit boundary; action tests need no terminal object.
//
// It holds the discovered set and the surface's one read of `.current`, and
// derives everything else from those two — the accounts whose registries
// gate a mutation, the current account, whether launches choose — so no two
// of them can describe different accounts.
type sessionActions struct {
	st           *state.Store
	set          accounts.Set
	routing      accounts.Routing
	cdFile       string
	claudeArgs   []string
	beforeLaunch func()

	// The edges a commit crosses, and where it reports what went wrong after
	// the picker has closed.
	place launchop.Deps
	exec  ExecFunc
	errw  io.Writer
	// failed is why a committed launch did not start.
	failed error
}

// auto says bare launches choose for themselves: there is then no current
// account, and wherever this surface would fall back to one it asks the
// placement rule instead.
func (actions *sessionActions) auto() bool {
	return actions.routing.Mode == "auto"
}

func (actions *sessionActions) resume(s *sessions.Session, override bool) (bool, error) {
	if actions.liveNow(s) == sessions.Live {
		return false, fmt.Errorf("open in another terminal — switch there instead")
	}
	if actions.cdFile != "" && strings.Contains(s.CWD, "\n") {
		return false, fmt.Errorf("project path contains a newline — not representable in the cd file")
	}
	// Asked now, not taken from the listing: a dir gone since it was drawn
	// refuses, one back since resumes, and an unrecovered cwd ("") fails the
	// stat like any other missing dir.
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
	req := launchop.Request{
		Set: actions.set, Store: actions.st, Intent: intent, Mode: "picker",
		Session: launchop.Session{Route: s.ID}, Owner: s.Owner,
		CWD: s.CWD, Environ: os.Environ(), Now: time.Now(),
	}
	if override {
		// The override moves the session, so its routing is part of the
		// launch: written with it, or the launch does not happen.
		req.Session.Record, req.MustReHome = s.ID, true
	}
	out, err := launchop.Gather(req, actions.place).Commit()
	if err != nil {
		return false, err
	}

	actions.beforeLaunch()
	out.Announce(actions.errw, "headroom sessions: ")
	recorded := ""
	if override {
		recorded = "; re-home remains recorded for " + out.Account().Name
	}
	if err := os.Chdir(s.CWD); err != nil {
		return true, fmt.Errorf("cd %s: %v%s", s.CWD, err, recorded)
	}
	if actions.cdFile != "" {
		if err := writeCDFile(actions.cdFile, s.CWD); err != nil {
			fmt.Fprintf(actions.errw, "headroom sessions: cd file: %v (continuing)\n", err)
		}
	}
	argv := append(append([]string{}, actions.claudeArgs...), "--resume", s.ID)
	// The launch surface's exec edge, which execs the path Prepare resolved
	// before the chdir above — so a relative PATH entry in the project
	// directory can never supply the binary.
	p := out.Prepared()
	if err := actions.exec(p.Path, p.Binary, argv, envWithPWD(p.Env, s.CWD)); err != nil {
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
	_, owned := accountNamed(actions.set, s.Owner)
	_, pinned := accountNamed(actions.set, actions.routing.Current)
	switch {
	case !override && owned:
		return placement.Intent{Kind: placement.Forced, Account: s.Owner, Reason: placement.ReasonOwner}, true
	case actions.auto() && override:
		return placement.Intent{Exclude: s.Owner}, true
	case actions.auto():
		return placement.Intent{}, true
	case pinned:
		// Degraded attribution — no evidence, or an owner the filesystem no
		// longer has — falls back to the *current* account; the row's owner
		// tag already says so.
		return placement.Intent{Kind: placement.Forced, Account: actions.routing.Current, Reason: "pinned"}, true
	}
	return placement.Intent{}, false
}

// accountNamed finds the set's account by name. A name the set does not hold
// — an owner whose dir is gone, the empty current of unresolvable routing —
// is not one this surface can route to.
func accountNamed(set accounts.Set, name string) (accounts.Account, bool) {
	for _, a := range set.Accounts {
		if name != "" && a.Name == name {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// liveNow re-establishes the selected session's liveness at action time —
// the listing's snapshot is display state, and a session opened since the
// picker drew must still gate every mutation and the resume refusal. The
// fresh answer also lands back on the row so the display tells the truth.
func (actions *sessionActions) liveNow(s *sessions.Session) sessions.LiveState {
	s.Live = sessions.LiveNow(s.ID, launchop.Refs(actions.set), actions.place.Probe)
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

// The two vendor mutations' refusals while the session may be open, said in
// the same words by the gate when the key is pressed and by the commit.
var (
	errOpenRename = errors.New("session is open elsewhere — rename there (Ctrl+R)")
	errOpenDelete = errors.New("session is open elsewhere — not deleting")
)

// mutable is the one gate on r and dd: a fresh registry sample, and refusal
// unless the session is verifiably not open — the vendor's writer may hold the
// file, and "could not tell" is not "closed". The picker asks when the key is
// pressed, and the commit asks again after the user has spent time editing a
// title or confirming deletion. refusal is the mutation's own word for it.
func (actions *sessionActions) mutable(s *sessions.Session, refusal error) error {
	if actions.liveNow(s) != sessions.NotLive {
		return refusal
	}
	return nil
}

func (actions *sessionActions) rename(s *sessions.Session, title string) error {
	if err := actions.mutable(s, errOpenRename); err != nil {
		return err
	}
	if err := sessions.AppendCustomTitle(s.Path, s.ID, title); err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}
	return nil
}

func (actions *sessionActions) delete(s *sessions.Session) (bool, error) {
	if err := actions.mutable(s, errOpenDelete); err != nil {
		return false, err
	}
	if err := sessions.DeleteTranscript(s); err != nil {
		return false, fmt.Errorf("delete failed: %w", err)
	}
	if err := actions.st.Forget(s.ID); err != nil {
		return true, fmt.Errorf("re-home cleanup failed: %w", err)
	}
	return true, nil
}
