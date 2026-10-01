package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchlog"
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
	placed       bool // this resume was placed by the rule, and so already recorded
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
	// One resume, one record: a refused attempt earlier in this picker session
	// must not make this one look already recorded.
	actions.placed = false
	acct, ok, why := actions.resumeAccount(s, override)
	if !ok {
		if why == "" {
			why = "no account to resume on — run headroom accounts"
		}
		return false, fmt.Errorf("%s", why)
	}
	prepared, err := launch.Prepare(acct, actions.set, os.Environ())
	if err != nil {
		return false, err
	}
	if override {
		err := actions.st.ReHome(s.ID, acct.Name, time.Now(), func() (map[string]bool, bool) { return sessions.TranscriptIDs(actions.cfg.StoreDir()) })
		if err != nil {
			return false, fmt.Errorf("re-home not recorded (%v) — enter resumes without it", err)
		}
	}
	actions.recordResume(s, acct)
	actions.beforeLaunch()
	for _, notice := range prepared.Notices {
		fmt.Fprintln(os.Stderr, "headroom sessions: "+notice)
	}
	recorded := ""
	if override {
		recorded = "; re-home remains recorded for " + acct.Name
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
	if err := execSessions(prepared.Path, prepared.Binary, argv, envWithPWD(prepared.Env, s.CWD)); err != nil {
		return true, fmt.Errorf("exec claude: %v%s", err, recorded)
	}
	return true, nil
}

// resumeAccount chooses the owner, then valid current. An override chooses
// current directly; an unresolved current leaves the action without a target.
//
// Under automatic placement there is no current account, and each place this
// would have fallen back to one asks the rule instead: a session with no
// evidence, or whose owner is gone, is placed like a new one, and the override
// moves the session to the least-loaded of the *other* accounts — moving means
// somewhere other than where it is. why says what refused, when the rule did.
func (actions *sessionActions) resumeAccount(s *sessions.Session, override bool) (acct accounts.Account, ok bool, why string) {
	find := func(name string) (accounts.Account, bool) {
		for _, a := range actions.set.Accounts {
			if a.Name == name {
				return a, name != ""
			}
		}
		return accounts.Account{}, false
	}
	if !override {
		if a, found := find(s.Owner); found {
			return a, true, ""
		}
	}
	if actions.auto {
		exclude := ""
		if override {
			exclude = s.Owner
		}
		return actions.place(s, exclude)
	}
	// The owner names an account the filesystem no longer has, or there is no
	// evidence at all: degraded attribution falls back to the *current*
	// account — the row's owner tag already says so — never to the primary,
	// which no evidence chose.
	a, found := find(actions.current)
	return a, found, ""
}

// recordResume records a resume the rule did not place — the owner's, or the
// pinned account's — so that it is load the next automatic launch counts and
// a line in the log, like every other session headroom starts. A resume the
// rule placed was recorded when it was placed. Nothing here can refuse: the
// account is already decided, and a record that fails is a record lost.
func (actions *sessionActions) recordResume(s *sessions.Session, acct accounts.Account) {
	if actions.placed || actions.st == nil {
		return
	}
	now := time.Now()
	facts := gatherPlacement(actions.set, actions.st, os.Environ(), false, now)
	placed, placeErr := actions.st.Place(facts.cands, placement.Intent{Kind: placement.Forced, Account: acct.Name, Reason: "picker"}, os.Getpid(), "", now)
	actions.log(s, placed, placeErr, now)
}

func (actions *sessionActions) log(s *sessions.Session, placed state.Placed, placeErr error, now time.Time) {
	if placed.Decision.Chosen == "" {
		return
	}
	rec := launchlog.New(placed.Decision, now)
	rec.Vendor, rec.PID, rec.Mode, rec.Session, rec.Recorded = string(actions.cfg.Vendor), os.Getpid(), "picker", s.ID, placed.Recorded
	rec.CWD = s.CWD
	if placeErr != nil {
		rec.Problem = placeErr.Error()
	}
	_ = launchlog.Append(actions.cfg.AccountsRoot, rec)
}

// place asks the placement rule where a resumed session goes, records the
// answer the way a launch does, and logs it. A bookkeeping failure costs the
// record and nothing else, as on the launch path.
func (actions *sessionActions) place(s *sessions.Session, exclude string) (accounts.Account, bool, string) {
	now := time.Now()
	facts := gatherPlacement(actions.set, actions.st, os.Environ(), true, now)
	placed, placeErr := actions.st.Place(facts.cands, placement.Intent{Exclude: exclude}, os.Getpid(), "", now)
	d := placed.Decision
	if d.Chosen == "" {
		return accounts.Account{}, false, d.Refusal
	}
	actions.placed = true
	actions.log(s, placed, placeErr, now)
	a, ok := facts.account(d.Chosen)
	return a, ok, ""
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
