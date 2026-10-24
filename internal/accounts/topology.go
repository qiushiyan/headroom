package accounts

// The shared-sessions topology is headroom's own invariant, not the shell's:
// each vendor keeps one machine-global session store — what the session
// picker lists for Claude Code, what `codex resume` finds on any home — and
// that holds only while every extra account's store link (Claude Code's
// projects/, Codex's sessions/) is a symlink to the canonical store. A
// violation forks session history silently: transcripts land in the
// account's private dir and vanish from every other account's view.
// Verification therefore lives here, shared by the launch gate and by check,
// so the gate and the report can never disagree. Creation is a distinct human
// act — `headroom accounts add` (lifecycle.go), built to satisfy this same
// check — and launch never seeds: a launch that quietly repaired topology
// would hide exactly the state this refusal exists to surface.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/qiushiyan/headroom/internal/config"
)

// VerifyTopology checks one non-primary account's shared-sessions link. The
// primary passes vacuously — it owns the canonical store. Each failure names
// the exact end state required: the repair is a manual move, and the
// message is the only runbook a refused launch has at hand.
//
// Classification is by Lstat, decision by inode identity: the two failure
// modes have different remedies (a link elsewhere is "fix by hand", a real
// directory holds unmigrated sessions), and comparing resolved paths as
// strings would break under symlinked homes.
func VerifyTopology(a Account) error {
	if a.IsPrimary() {
		return nil
	}
	canon := a.Scope.StoreDir()
	link := a.Dir() + "/" + a.Scope.StoreLink()

	switch absent, err := canonicalStore(a.Scope); {
	case err != nil:
		return err
	case absent:
		return fmt.Errorf("canonical session store %s does not exist — %s must be a symlink to it; create the store (mkdir); `%s` seeds new accounts", canon, link, addCommand(a.Scope))
	}

	lfi, err := os.Lstat(link)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("%s is missing — it must be a symlink to %s (`%s` seeds new accounts with it)", link, canon, addCommand(a.Scope))
	case err != nil:
		return fmt.Errorf("%s unreadable (%v)", link, err)
	case lfi.Mode()&os.ModeSymlink != 0:
		rcanon, err1 := os.Stat(canon)
		rlink, err2 := os.Stat(link)
		if err1 == nil && err2 == nil && os.SameFile(rcanon, rlink) {
			return nil
		}
		return fmt.Errorf("%s is a symlink but does not resolve to %s — fix it by hand", link, canon)
	case lfi.IsDir():
		return fmt.Errorf("%s is a real directory (unmigrated sessions?) — with no %s running, move its contents into %s, keeping a file both hold only when the two are identical (different contents are two histories: stop and reconcile them by hand), then replace it with a symlink to %s", link, a.Scope.Binary(), canon, canon)
	default:
		return fmt.Errorf("%s is not a symlink to %s — fix it by hand", link, canon)
	}
}

// canonicalStore classifies the vendor's canonical session store by Lstat,
// for the launch gate and for seeding alike. Absent is reported apart, as the
// one state seeding may repair itself; anything else that is not a real
// directory is the violation, named here once.
func canonicalStore(s config.Scope) (absent bool, err error) {
	canon := s.StoreDir()
	fi, err := os.Lstat(canon)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("canonical session store %s unreadable (%v)", canon, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("canonical session store %s is itself a symlink — it must be a real directory, or every account's history chains through it", canon)
	case !fi.IsDir():
		return false, fmt.Errorf("canonical session store %s is not a directory", canon)
	}
	return false, nil
}

// addCommand is the seeding command's spelling for a scope.
func addCommand(s config.Scope) string {
	if s.Vendor == config.Codex {
		return "headroom accounts add --vendor codex"
	}
	return "headroom accounts add"
}
