package app

// `headroom accounts add` / `headroom accounts remove`: the two commands that
// change the account set. The board is the noun these hang off — the same
// collection, grown or shrunk. Filesystem work is accounts.Seed/RemoveDir;
// this file owns the edges: flags, the confirmation, the liveness gate
// (process inspection) and the Keychain deletion (`security`), in that
// order, so nothing irreversible runs before every refusal has had its say.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// AddOptions are a new account: its email, and what of a config to link
// into its dir.
type AddOptions struct {
	Email string
	// ShareConfig links the primary's config (settings, skills, …) into the
	// new dir; with ShareFrom, every entry of that dir instead.
	ShareConfig bool
	ShareFrom   string
}

// AccountsAdd seeds the dir for a new subscription: its session store linked
// to the machine-global one, and, when asked, a config linked from another
// dir. It is the one command that works while a vendor is absent: it is how
// the first Codex directory comes to exist.
func AccountsAdd(std Streams, cfg config.Scope, opts AddOptions) error {
	out, name := std.Out, opts.Email
	opt := accounts.SeedOptions{}
	switch {
	case opts.ShareFrom != "":
		// That dir, every entry: a config package holds config and nothing
		// else.
		from := opts.ShareFrom
		if !filepath.IsAbs(from) {
			if abs, err := filepath.Abs(from); err == nil {
				from = abs
			}
		}
		opt.ShareFrom, opt.ShareNames = from, nil
	case opts.ShareConfig:
		// The primary's config, whitelisted.
		opt.ShareFrom, opt.ShareNames = cfg.PrimaryDir(), accounts.SharedConfigEntries(cfg.Vendor)
	}
	seeded, err := accounts.Seed(cfg, name, opt)
	if err != nil {
		return err
	}
	dir := seeded.Dir
	fmt.Fprintf(out, "seeded %s\n", dir)
	fmt.Fprintf(out, "  %s → %s (sessions are machine-global)\n", cfg.StoreLink(), cfg.StoreDir())
	if opt.ShareFrom != "" {
		if len(seeded.Shared) == 0 {
			fmt.Fprintf(out, "  shared nothing from %s (no matching entries)\n", opt.ShareFrom)
		} else {
			fmt.Fprintf(out, "  shared from %s: %s\n", opt.ShareFrom, strings.Join(seeded.Shared, ", "))
		}
	}
	if len(seeded.Settings) > 0 {
		fmt.Fprintf(out, "  shared from %s: %s\n", cfg.PrimaryDir(), strings.Join(seeded.Settings, ", "))
	}
	if len(seeded.Kept) > 0 {
		// A login and its history are never shared, whatever the source
		// holds: this dir gets its own at its first login.
		fmt.Fprintf(out, "  kept per dir, not linked: %s\n", strings.Join(seeded.Kept, ", "))
	}
	if cfg.Vendor == config.Codex {
		// Codex has no in-session /login: logging in is one command, in the
		// engine's own spelling — a wrapper's argument passing is not
		// headroom's to know.
		fmt.Fprintf(out, "next: headroom launch --vendor codex --account %s -- login   and log in as %s\n", name, name)
	} else {
		fmt.Fprintf(out, "next: headroom launch --account %s   then /login as %s\n", name, name)
	}
	fmt.Fprintf(out, "      (the board warns if the login does not match the dir name; new accounts sort last until listed in %s)\n", cfg.OrderFile())
	return nil
}

// removeDeps are the two edges removal crosses, injected so the command's
// ordering — gate, confirm, Keychain, dir — is testable without a Keychain
// or live processes.
type removeDeps struct {
	probe          sessions.PIDProbe
	deleteKeychain func(configDir string) (bool, error)
	stdin          io.Reader
	interactive    bool
}

// AccountsRemove removes one extra account: its Keychain item and its dir,
// refused while a session is live on it or its liveness cannot be verified,
// after a confirmation unless yes. A bare removal names what is removable and
// refuses. Codex accounts are never removed: headroom cannot tell whether a
// codex session is running on a home.
func AccountsRemove(std Streams, term Terminal, cfg config.Scope, name string, yes bool, probe sessions.PIDProbe) error {
	if cfg.Vendor == config.Codex {
		return codexRemoveRefusal(cfg, name)
	}
	return accountsRemove(std, cfg, name, yes, removeDeps{
		probe:          probe,
		deleteKeychain: creds.DeleteKeychainItem,
		stdin:          std.In,
		// Both ends: the y/N prompt is printed on stdout, so a redirected
		// stdout with a tty stdin would wait on a question nobody can see.
		interactive: term.In && term.Out,
	})
}

// codexRemoveRefusal is the whole of Codex removal in this version. Claude
// Code's removal is gated on a live-session registry; Codex has none headroom
// can read, and an empty read must not pass as proof of inactivity — deleting
// a home under a running codex orphans its login and its index. So the
// refusal is decided here, in plain sight, before any of the removal
// machinery runs, with and without --yes, and it names the directory so the
// cleanup can be done by hand.
func codexRemoveRefusal(cfg config.Scope, name string) error {
	const why = "headroom cannot tell whether a Codex session is running on a home, so it never removes one"
	if name == "" || name != filepath.Base(name) {
		return fmt.Errorf("%s — delete the account's directory under %s by hand once no codex process runs", why, cfg.AccountsRoot)
	}
	return fmt.Errorf("%s — delete %s by hand once no codex process runs", why, filepath.Join(cfg.AccountsRoot, name))
}

// removeCandidate is one thing `accounts remove` will accept by name: an
// extra account dir (named by its login) or stranded `<name>.lock` debris.
// The primary is never a candidate.
type removeCandidate struct {
	Name  string
	Email string // what the dir's .claude.json reports ("" = never logged in)
	Lock  bool   // vendor lock debris, not an account
}

// removeCandidates lists what the command can remove, in board order
// (discovery's extras, then lock debris by name). Discovery skips `.lock`
// dirs by design, so they come from accounts.LockDebris. An accounts root
// that does not exist is an empty list; one that cannot be read is returned
// as the error, so "nothing to remove" is never said over a root that was
// merely unreadable.
func removeCandidates(cfg config.Scope) ([]removeCandidate, error) {
	var cands []removeCandidate
	for _, a := range accounts.Discover(cfg).Accounts {
		if a.IsPrimary() {
			continue
		}
		cands = append(cands, removeCandidate{Name: a.Name, Email: a.Email})
	}
	debris, err := accounts.LockDebris(cfg)
	if err != nil {
		return cands, fmt.Errorf("listing %s: %w", cfg.AccountsRoot, err)
	}
	for _, name := range debris {
		cands = append(cands, removeCandidate{Name: name, Lock: true})
	}
	return cands, nil
}

func (c removeCandidate) describe() string {
	switch {
	case c.Lock:
		return "lock debris"
	case c.Email == "":
		return "never logged in"
	case c.Email == c.Name:
		return "logged in"
	default:
		return "logged in as " + c.Email
	}
}

// removable says what a removal could name: the candidates, or why there
// are none to name.
func removable(cands []removeCandidate, err error) string {
	if err != nil {
		return err.Error()
	}
	if len(cands) == 0 {
		return "nothing to remove: no extra accounts under the accounts root"
	}
	var b strings.Builder
	b.WriteString("removable:")
	for _, c := range cands {
		fmt.Fprintf(&b, "\n  %s  (%s)", c.Name, c.describe())
	}
	return b.String()
}

func accountsRemove(std Streams, cfg config.Scope, name string, yes bool, deps removeDeps) error {
	out := std.Out
	if name == "" {
		// Removal names its object up front: a usage error that names the
		// choices.
		cands, err := removeCandidates(cfg)
		return usagef("name the account to remove — %s", removable(cands, err))
	}
	// The primary is not removable: it is Claude Code's default dir, not an
	// entry in the accounts root, and its name is not a dir name at all.
	accts := accounts.Discover(cfg).Accounts
	var target *accounts.Account
	for i := range accts {
		if accts[i].Name == name {
			target = &accts[i]
			break
		}
	}
	if target != nil && target.IsPrimary() {
		return fmt.Errorf("%q is the primary ~/.claude — Claude Code's own default dir is never removed by headroom", name)
	}
	dir := filepath.Join(cfg.AccountsRoot, name)
	// A name nothing answers to — a typo, or an account already gone — gets
	// the list of what would have worked, not just a missing-path error.
	if _, err := os.Lstat(dir); err != nil && os.IsNotExist(err) {
		cands, err := removeCandidates(cfg)
		return fmt.Errorf("no account named %q — %s", name, removable(cands, err))
	}
	// Every filesystem refusal RemoveDir will make, before anything else —
	// including the one that guards data: a real projects/ directory holds
	// sessions never migrated into the store.
	if err := accounts.CheckRemovable(cfg, name); err != nil {
		return err
	}

	// Gate: a session this dir is driving right now. Verified live refuses,
	// and so does "could not verify":
	// deleting a config dir under a running claude orphans its login state.
	// Strict read: a registry file that does not parse, or a sessions/ dir
	// that cannot be listed, is "could not verify", not "nothing there".
	gate := func() error {
		reg := sessions.ReadRegistry(name, dir)
		if len(reg.Problems) > 0 {
			return fmt.Errorf("%s: live-session registry unreadable (%v) — refusing while liveness cannot be verified", name, reg.Problems)
		}
		claims, _ := sessions.InspectClaims(reg.Entries, deps.probe)
		for id, evidence := range claims {
			switch evidence.State {
			case sessions.Live:
				return fmt.Errorf("%s has a live session (%s) — quit it first", name, id)
			case sessions.LiveUnknown:
				return fmt.Errorf("%s has a registered session (%s) whose liveness could not be verified — quit any claude on this account and retry", name, id)
			}
		}
		return nil
	}
	if err := gate(); err != nil {
		return err
	}

	login := ""
	if target != nil {
		login = target.Email
	}
	if !yes {
		if !deps.interactive {
			return usagef("refusing without confirmation off a terminal — pass --yes")
		}
		if login != "" {
			fmt.Fprintf(out, "remove %s (logged in as %s)?\n", name, login)
		} else {
			fmt.Fprintf(out, "remove %s?\n", name)
		}
		fmt.Fprintf(out, "  deletes %s and its Keychain item; transcripts survive\n", dir)
		fmt.Fprintf(out, "[y/N] ")
		reply, _ := bufio.NewReader(deps.stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(reply)) {
		case "y", "yes":
		default:
			return errors.New("aborted")
		}
	}

	// The prompt may have stayed open for minutes; a session started
	// meanwhile must refuse the same way. The gate runs again immediately
	// before the first irreversible step.
	if !yes {
		if err := gate(); err != nil {
			return err
		}
	}

	// Keychain before dir: the service name is derived from the dir's
	// spelling, not its existence, so either order works — but if the
	// deletion fails, the dir is still there to retry against.
	deleted, err := deps.deleteKeychain(dir)
	switch {
	case err != nil:
		return fmt.Errorf("%v — dir left in place", err)
	case deleted:
		fmt.Fprintf(out, "deleted Keychain item (%s)\n", creds.ServiceName(dir))
	default:
		fmt.Fprintf(out, "no Keychain item (%s) — never logged in, or already gone\n", creds.ServiceName(dir))
	}
	if err := accounts.RemoveDir(cfg, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", dir)
	// The account is gone. What it left elsewhere is tidied step by step,
	// each on its own: one that fails does not keep the others from running,
	// since a re-run finds no account to remove and would never reach them.
	// Whatever is left is named with what clears it by hand.
	var left []string
	if err := accounts.ScrubOrder(cfg, name); err != nil {
		left = append(left, fmt.Sprintf("its line in %s (%v) — delete the line by hand", cfg.OrderFile(), err))
	}
	// A secret for an account that no longer exists is no use to anyone.
	// (Only Claude Code accounts reach here: Codex removal refuses up front.)
	if forgot, err := headless.Remove(cfg.AccountsRoot, name); err != nil {
		left = append(left, fmt.Sprintf("its headless token (%v) — headroom headless forget %s", err, name))
	} else if forgot {
		fmt.Fprintln(out, "forgot its headless token — headroom does not revoke it with the vendor")
	}
	if err := accounts.SetReservation(cfg, name, nil); err != nil {
		left = append(left, fmt.Sprintf("its reservation (%v) — headroom accounts reserve %s --clear", err, name))
	}
	fmt.Fprintln(out, "transcripts are machine-global and survive; a session it drove is placed like a new one when it is next resumed")
	if len(left) > 0 {
		return fmt.Errorf("%s is removed, but not everything of it was cleared: %s", name, strings.Join(left, "; "))
	}
	return nil
}
