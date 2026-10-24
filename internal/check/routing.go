package check

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/state"
)

// checkRouting audits the launch-routing state: the `.current` selection and
// the process environment. It runs for every vendor unconditionally — nothing
// here reads state.json, so no state-schema condition may suppress it.
func checkRouting(set accounts.Set, environ []string, r *report) {
	cfg, accts := set.Scope, set.Accounts

	// The launch path's own resolver, applied exactly as `headroom launch`
	// applies it: absent is the documented fresh-start default, while empty,
	// unreadable or naming a deleted account is corrupt routing state — launch
	// refuses on it, so the board is where the user hears about it first only
	// if this line is missing.
	bare, err := set.Bare()
	label := "current: .current resolves to a launchable account"
	switch {
	case err != nil:
	case bare.Auto:
		// Automatic placement is a valid routing state, not a missing one:
		// there is no single account, and a bare launch chooses.
		label = "current: .current says auto — a bare launch chooses the least-loaded account"
	default:
		label = fmt.Sprintf("%s (%s)", label, bare.Account.Name)
	}
	r.own(err == nil, label, fmt.Sprintf("%v — headroom launch refuses until it is fixed", err))

	// Not a failure and not drift: an inherited CLAUDE_CONFIG_DIR (a tmux
	// server started inside a Claude Code session, a nested shell) is
	// neutralized by every managed launch and every health probe headroom
	// spawns. It is reported because tools *outside* headroom that read the
	// variable are still being steered by it. Presence is the condition —
	// a present-but-empty value is unverified vendor territory, not the
	// verified absent state, so it is reported too. A *relative* value is the
	// stale-wrapper incident's signature and does fail: every unmanaged
	// `claude` in that shell runs as the primary while writing state beside
	// whatever directory it happens to start in (verified 2.1.220). Codex
	// resolves a relative CODEX_HOME against the cwd like any path, so there
	// it is reported, never failed.
	homeVar := cfg.Env().HomeVar
	if v, present := launch.Ambient(cfg.Vendor, environ); present {
		if v != "" && !filepath.IsAbs(v) && cfg.Vendor == config.Claude {
			// Present-but-empty stays the ok-with-detail line below: it is
			// unverified vendor territory, not this signature.
			r.own(false, fmt.Sprintf("env: inherited CLAUDE_CONFIG_DIR is relative (%q)", v),
				"the stale-wrapper signature — unmanaged claude runs as the primary while writing state beside the cwd; exec zsh")
		} else {
			r.ok(fmt.Sprintf("env: inherited %s is neutralized by managed launches (%q)", homeVar, v))
		}
	}

	// The credential-redirect variable gets the same reporting: managed
	// launches strip it (obeying it would pair one account's tokens with
	// another's state — verified 2.1.220), unmanaged tools still obey it.
	for _, in := range launch.Redirects(cfg.Vendor, environ) {
		// A credential variable is named, never quoted.
		r.ok(fmt.Sprintf("env: inherited %s is neutralized by managed launches", in.String()))
	}

	// Every discovered config dir must be absolute: config.Load refuses
	// relative roots and launch.Extra refuses relative dirs, so a relative
	// path here means a construction this binary no longer performs — or a
	// bug in it.
	badDir := ""
	for _, a := range accts {
		if a.ConfigDir != "" && !filepath.IsAbs(a.ConfigDir) {
			badDir = a.ConfigDir
			break
		}
	}
	r.own(badDir == "", "dirs: every account config dir is absolute",
		fmt.Sprintf("%q is relative — claude would run it as the primary", badDir))

	// A relocated home (HEADROOM_HOME) re-points what headroom observes but
	// not what a primary launch would use — the primary is selected by the
	// vendor's home variable being absent, resolved by the vendor against the
	// real home, for Codex exactly as for Claude Code. Own-state, not drift:
	// launch refuses the primary until it is unset.
	if cfg.PrimaryLaunchRefused() {
		r.own(false, "home: HEADROOM_HOME re-points the primary headroom describes",
			fmt.Sprintf("primary launches refuse; the board describes a tree bare `%s` would not use", cfg.Binary()))
	}

	// Vendor lock debris in the accounts root: Claude Code's config locking
	// creates `<dir>.lock` directories, and a crash strands them where
	// discovery would otherwise adopt one as an account (observed 2026-08-10;
	// the health probe had seeded a skeleton .claude.json inside). Discovery
	// skips them; this line is where a stranded one is named instead of
	// silently vanishing from every surface.
	if debris, err := accounts.LockDebris(cfg); err == nil {
		for _, name := range debris {
			r.ok(fmt.Sprintf("root: %s is vendor lock debris, not an account (skipped by discovery)", name))
		}
	}

	// The shared-sessions topology is headroom's own invariant — every
	// account sees one machine-global session store only while every extra's
	// store link is a symlink to the canonical store — and launch refuses on
	// its violation, so it is asserted here with the same shared verifier, per
	// account: accounts fail independently.
	for _, a := range accts {
		if a.IsPrimary() {
			continue
		}
		err := accounts.VerifyTopology(a)
		hint := ""
		if err != nil {
			hint = err.Error() + " — launches on this account refuse until it is fixed"
		}
		r.own(err == nil, fmt.Sprintf("topology[%s]: %s/ resolves to the canonical store", a.Name, cfg.StoreLink()), hint)
	}
}

// checkHomes audits what a second home on this machine rests on: how this
// home's primary is selected, that no dir shares another's login, which ledger
// this home spends against, and that no account dir belongs to two homes.
// Every failure here is headroom's own state — a dir it seeded, a file it
// reads — never the vendor's.
func checkHomes(set accounts.Set, snap state.Snapshot, r *report) {
	scope := set.Scope
	if scope.PrimaryExplicit {
		r.ok(fmt.Sprintf("home: HOME is not this user's login home — the primary is launched by its dir (%s), never by absence", scope.PrimaryDir()))
	}

	// A login file linked between two dirs makes them one login, and a linked
	// history or registry puts one account's sessions under another's name:
	// every entry no share mode links (accounts.PerDirEntries) but the store
	// link, which is a link by design. Only dirs headroom launches by their
	// dir are judged: the login home's primary keeps ~/.claude.json outside
	// its dir, where it is the vendor's.
	var linked []string
	for _, a := range set.Accounts {
		if a.ConfigDir == "" {
			continue
		}
		for _, entry := range accounts.PerDirEntries(scope.Vendor) {
			if entry == scope.StoreLink() {
				continue
			}
			path := filepath.Join(a.Dir(), entry)
			if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				linked = append(linked, path)
			}
		}
	}
	r.own(len(linked) == 0, "logins: every account dir keeps its own login, history and session registry",
		fmt.Sprintf("%s is a link — a login shared between dirs is one login under two names; remove the link and log that dir in again", strings.Join(linked, ", ")))

	var shared []string
	mine := map[string]os.FileInfo{}
	for _, a := range set.Accounts {
		if fi, err := os.Stat(a.Dir()); err == nil {
			mine[a.Dir()] = fi
		}
	}
	homes := snap.OtherHomes(scope)
	for _, h := range homes {
		for _, a := range h.Set.Accounts {
			fi, err := os.Stat(a.Dir())
			if err != nil {
				continue
			}
			for dir, ofi := range mine {
				if os.SameFile(fi, ofi) {
					shared = append(shared, fmt.Sprintf("%s (also %s's)", dir, h.Root))
				}
			}
		}
	}
	if snap.Shared() || len(homes) > 0 {
		whose := "this home's own"
		if snap.Shared() {
			whose = "another home's"
		}
		r.ok(fmt.Sprintf("ledger: %d other home(s) spend against this ledger (%s) — their sessions and launches count here", len(homes), whose))
		r.own(len(shared) == 0, "homes: no account dir belongs to two homes",
			strings.Join(shared, ", ")+" — a launch in one home would run on the other's dir")
	}
}
