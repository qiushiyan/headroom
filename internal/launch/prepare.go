package launch

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

// Prepared holds all predictable launch decisions, resolved before the caller
// records a choice or changes cwd. Persistence and exec remain caller actions.
type Prepared struct {
	Path    string
	Env     []string
	Notices []string
}

func Prepare(cfg config.Config, account accounts.Account, discovered []accounts.Account, base []string) (Prepared, error) {
	var p Prepared
	if account.IsPrimary() && cfg.PrimaryRelocated {
		return p, fmt.Errorf("HEADROOM_HOME re-points the primary — cannot launch it here (extras are unaffected)")
	}
	if err := accounts.VerifyTopology(cfg, account); err != nil {
		return p, fmt.Errorf("not launching — %w", err)
	}
	target, err := For(account.ConfigDir)
	if err != nil {
		return p, err
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return p, fmt.Errorf("claude not found on PATH: %w", err)
	}
	// LookPath can return a relative path containing a slash. Anchor it before
	// the sessions caller enters another project directory.
	p.Path, err = filepath.Abs(path)
	if err != nil {
		return p, err
	}
	p.Env = target.Env(base)
	if value, conflict := target.Conflicts(base); conflict && !accounts.KnownExtraDir(discovered, value) {
		p.Notices = append(p.Notices, fmt.Sprintf("ignoring inherited CLAUDE_CONFIG_DIR=%s; launching %s (%s)", value, account.Name, account.Dir(cfg)))
	}
	if value, present := AmbientSecureStorage(base); present {
		p.Notices = append(p.Notices, "ignoring inherited CLAUDE_SECURESTORAGE_CONFIG_DIR="+value)
	}
	return p, nil
}
