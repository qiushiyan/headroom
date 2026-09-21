package launch

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/qiushiyan/headroom/internal/accounts"
)

// Prepared holds all predictable launch decisions, resolved before the caller
// records a choice or changes cwd. Persistence and exec remain caller actions.
type Prepared struct {
	Path    string
	Binary  string // argv[0]
	Env     []string
	Notices []string
}

// Prepare takes the account and the set it was selected from; the binary, the
// stripped variables and the notices all come from the account's own scope.
func Prepare(account accounts.Account, discovered accounts.Set, base []string) (Prepared, error) {
	var p Prepared
	scope := account.Scope
	if discovered.Scope.Vendor != scope.Vendor {
		return p, fmt.Errorf("%s account %q was not selected from the %s accounts", scope.Vendor.Title(), account.Name, discovered.Scope.Vendor.Title())
	}
	if account.IsPrimary() && scope.PrimaryRelocated {
		return p, fmt.Errorf("HEADROOM_HOME re-points the primary — cannot launch it here (extras are unaffected)")
	}
	if err := accounts.VerifyTopology(account); err != nil {
		return p, fmt.Errorf("not launching — %w", err)
	}
	target, err := For(scope.Vendor, account.ConfigDir)
	if err != nil {
		return p, err
	}
	p.Binary = scope.Binary()
	path, err := exec.LookPath(p.Binary)
	if err != nil {
		return p, fmt.Errorf("%s not found on PATH: %w", p.Binary, err)
	}
	// LookPath can return a relative path containing a slash. Anchor it before
	// the sessions caller enters another project directory.
	p.Path, err = filepath.Abs(path)
	if err != nil {
		return p, err
	}
	p.Env = target.Env(base)
	if value, conflict := target.Conflicts(base); conflict && !discovered.KnownExtraDir(value) {
		p.Notices = append(p.Notices, fmt.Sprintf("ignoring inherited %s=%s; launching %s (%s)", scope.Env().HomeVar, value, account.Name, account.Dir()))
	}
	for _, in := range Redirects(scope.Vendor, base) {
		p.Notices = append(p.Notices, "ignoring inherited "+in.String())
	}
	return p, nil
}
