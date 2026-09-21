package accounts

// The shared-sessions topology is headroom's own invariant, not the shell's:
// `headroom resume`'s whole session model — one picker over one machine-global
// store — holds only while every extra account's projects/ is a symlink to the
// canonical store. A violation forks session history silently: transcripts
// land in the account's private dir and vanish from the picker. Verification
// therefore lives here, shared by the launch gate and by check, so the gate
// and the report can never disagree. Creation is a distinct human act —
// `headroom accounts add` (lifecycle.go), built to satisfy this same check —
// and launch never seeds: a launch that quietly repaired topology would hide
// exactly the state this refusal exists to surface.

import (
	"fmt"
	"os"

	"github.com/qiushiyan/headroom/internal/config"
)

// VerifyTopology checks one non-primary account's shared-sessions link. The
// primary passes vacuously — it owns the canonical store. Each failure names
// the exact end state required, not just a repair command: the repair lives
// in another repo's tooling, and a message that only names the command
// strands anyone on a machine without it.
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

	cfi, err := os.Lstat(canon)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("canonical session store %s does not exist — %s must be a symlink to it; create the store (mkdir); `%s` seeds new accounts", canon, link, addCommand(a.Scope))
	case err != nil:
		return fmt.Errorf("canonical session store %s unreadable (%v)", canon, err)
	case cfi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("canonical session store %s is itself a symlink — it must be a real directory, or every account's history chains through it", canon)
	case !cfi.IsDir():
		return fmt.Errorf("canonical session store %s is not a directory", canon)
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
		return fmt.Errorf("%s is a real directory (unmigrated sessions?) — move its contents into %s and replace it with a symlink there (with no %s running); it must be a symlink to %s", link, canon, a.Scope.Binary(), canon)
	default:
		return fmt.Errorf("%s is not a symlink to %s — fix it by hand", link, canon)
	}
}

// addCommand is the seeding command's spelling for a scope.
func addCommand(s config.Scope) string {
	if s.Vendor == config.Codex {
		return "headroom accounts add --vendor codex"
	}
	return "headroom accounts add"
}
