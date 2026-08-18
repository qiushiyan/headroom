package accounts

// Account lifecycle: seeding a dir for a new subscription and removing one.
// These are the two human acts that change the account set — the filesystem
// is the registry, so "add an account" *is* "make the dir the topology
// verifier will accept", and the verifier's own error messages name this
// command. Seeding therefore lives beside VerifyTopology, under the same
// rules (the canonical store is a real directory, every extra's projects/ is
// a symlink to it, never forced), so the two can never disagree about what
// a well-formed account looks like. Launch still never seeds: a launch that
// quietly repaired topology would hide exactly the state the refusal exists
// to surface.
//
// What is personal stays out: which config to share is a caller choice
// (SeedOptions), and Keychain deletion is an exec edge the app layer owns.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/config"
)

// SharedConfigEntries is the default set an account may share with the
// primary's ~/.claude when the caller asks for sharing without naming a
// source: configuration only. Deliberately a whitelist, not "everything
// but": a config dir also holds per-login and per-run state that must stay
// per account — history.jsonl is session-ownership evidence, sessions/ is
// the live registry, .credentials.json is a login where no Keychain
// exists — and a blacklist would silently share whichever of those the
// vendor adds next. This list is vendor-perishable, but nothing load-bearing
// rides on it: a missing entry means one skill or setting is absent in that
// account, never a misroute, which is why check does not verify it.
var SharedConfigEntries = []string{
	"settings.json",
	"CLAUDE.md",
	"keybindings.json",
	"skills",
	"commands",
	"agents",
	"hooks",
	"plugins",
	"output-styles",
}

// SeedOptions selects what a fresh account dir shares. ShareFrom "" shares
// nothing — the account starts on Claude Code's defaults. Otherwise every
// entry named in ShareNames is symlinked from ShareFrom into the account
// dir; a nil ShareNames means *every* entry of ShareFrom (the "dotfiles
// package" case, where the source dir is config and nothing else).
type SeedOptions struct {
	ShareFrom  string
	ShareNames []string
}

// ValidateExtraName is the naming rule for a dir under the accounts root:
// an email (contains "@"), a single path element, never lock debris. Every
// extra is named by its login so the board can catch a mismatched /login;
// discovery would adopt anything, so the rule is enforced where dirs are
// made.
func ValidateExtraName(name string) error {
	switch {
	case name == "":
		return errors.New("account name is empty")
	case strings.ContainsAny(name, `/\`), name != filepath.Base(name):
		return fmt.Errorf("%q is a path, not an account name — accounts are named by email", name)
	case LockArtifact(name):
		return fmt.Errorf("%q ends in .lock — that is vendor lock debris, never an account name", name)
	case !strings.Contains(name, "@"):
		return fmt.Errorf("%q is not an email — an extra account is named by the email it will log in as", name)
	}
	return nil
}

// Seed creates the dir for a new extra account: makes it, links its
// projects/ to the canonical session store (creating the store if it does
// not exist yet), symlinks whatever opt says to share, and verifies the
// result with the same topology check launch applies. It refuses if the dir
// already exists and never forces a link over anything.
//
// On failure after the dir was made, the partial dir is left in place with
// the error naming it: a half-seeded dir is inert (discovery lists it, the
// board shows it as never logged in, launch refuses it on topology) and
// deleting someone's directory to tidy up is not this function's call.
func Seed(cfg config.Config, name string, opt SeedOptions) (dir string, shared []string, err error) {
	if err := ValidateExtraName(name); err != nil {
		return "", nil, err
	}
	dir = filepath.Join(cfg.AccountsRoot, name)
	if _, err := os.Lstat(dir); err == nil {
		return dir, nil, fmt.Errorf("%s already exists", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return dir, nil, fmt.Errorf("%s: %v", dir, err)
	}
	if err := ensureCanonicalStore(cfg); err != nil {
		return dir, nil, err
	}
	if opt.ShareFrom != "" {
		if !filepath.IsAbs(opt.ShareFrom) {
			return dir, nil, fmt.Errorf("share source %q is not absolute", opt.ShareFrom)
		}
		if fi, err := os.Stat(opt.ShareFrom); err != nil || !fi.IsDir() {
			return dir, nil, fmt.Errorf("share source %s is not a directory", opt.ShareFrom)
		}
	}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		return dir, nil, err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return dir, nil, err
	}
	if err := os.Symlink(cfg.ProjectsDir(), filepath.Join(dir, "projects")); err != nil {
		return dir, nil, fmt.Errorf("linking projects: %v (partial dir left at %s)", err, dir)
	}
	if opt.ShareFrom != "" {
		names := opt.ShareNames
		if names == nil {
			entries, err := os.ReadDir(opt.ShareFrom)
			if err != nil {
				return dir, nil, fmt.Errorf("reading %s: %v (partial dir left at %s)", opt.ShareFrom, err, dir)
			}
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		for _, n := range names {
			if n == "projects" || strings.ContainsAny(n, `/\`) || n == "." || n == ".." {
				continue // projects is the store link; anything else odd is not a config entry
			}
			src := filepath.Join(opt.ShareFrom, n)
			if _, err := os.Lstat(src); err != nil {
				continue // a whitelist entry the source does not have: nothing to share
			}
			if err := os.Symlink(src, filepath.Join(dir, n)); err != nil {
				return dir, shared, fmt.Errorf("linking %s: %v (partial dir left at %s)", n, err, dir)
			}
			shared = append(shared, n)
		}
	}
	a := Account{ConfigDir: dir, Name: name}
	if err := VerifyTopology(cfg, a); err != nil {
		return dir, shared, fmt.Errorf("seeded dir failed the topology check it was built to pass: %v", err)
	}
	return dir, shared, nil
}

// ensureCanonicalStore makes ~/.claude/projects exist as a real directory —
// the one precondition seeding may satisfy itself, because an absent store
// on a fresh machine is not a fork of anyone's history. A store that exists
// as a symlink or a file is a topology violation and refuses, same as
// VerifyTopology would.
func ensureCanonicalStore(cfg config.Config) error {
	canon := cfg.ProjectsDir()
	fi, err := os.Lstat(canon)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(canon, 0o755)
	case err != nil:
		return fmt.Errorf("canonical session store %s unreadable (%v)", canon, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("canonical session store %s is itself a symlink — it must be a real directory", canon)
	case !fi.IsDir():
		return fmt.Errorf("canonical session store %s is not a directory", canon)
	}
	return nil
}

// RemoveDir deletes an extra account's dir (or a stranded `<name>.lock`)
// and scrubs its `.order` line. It is the filesystem half of removal only:
// the liveness gate and the Keychain deletion happen before it, in the app
// layer, because both are edges (process inspection, `security`) and both
// must precede the point of no return.
//
// os.RemoveAll does not follow symlinks, so the account's projects/ link
// goes and the canonical store it points at is untouched — the property the
// tests pin, since it is the difference between removing an account and
// deleting every session on the machine.
//
// `.current` is deliberately not rewritten: it is headroom's routing fact
// with a fail-closed reader, and a removed current account makes launch
// refuse until the board repicks — corrupt-vs-chosen stays distinguishable.
func RemoveDir(cfg config.Config, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return fmt.Errorf("%q is a path, not an account name", name)
	}
	if !LockArtifact(name) && !strings.Contains(name, "@") {
		return fmt.Errorf("%q is neither an email nor lock debris", name)
	}
	dir := filepath.Join(cfg.AccountsRoot, name)
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%s: %v", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// A symlinked account dir is not something headroom made; removing
		// the link would be fine, but following it would not, and RemoveAll
		// on a link removes just the link. Refuse anyway: whoever linked it
		// knows what is behind it, and this command should not guess.
		return fmt.Errorf("%s is a symlink — remove it by hand", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return scrubOrder(cfg, name)
}

// scrubOrder drops name's line from .order, if present, keeping every other
// byte of the file (comments, spacing) as the human wrote it.
func scrubOrder(cfg config.Config, name string) error {
	data, err := os.ReadFile(cfg.OrderFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	changed := false
	for _, l := range lines {
		if strings.TrimSpace(l) == name {
			changed = true
			continue
		}
		kept = append(kept, l)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(cfg.OrderFile(), []byte(strings.Join(kept, "\n")), 0o644)
}
