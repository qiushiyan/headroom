package app

// `headroom accounts add` / `headroom accounts remove`: the two commands that
// change the account set. The board is the noun these hang off — the same
// collection, grown or shrunk. Filesystem work is accounts.Seed/RemoveDir;
// this file owns the edges: flags, the confirmation, the liveness gate
// (process inspection) and the Keychain deletion (`security`), in that
// order, so nothing irreversible runs before every refusal has had its say.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/sessions"
	"golang.org/x/term"
)

func runAccountsAdd(cfg config.Config, args []string) int {
	return runAccountsAddTo(os.Stdout, os.Stderr, cfg, args)
}

func runAccountsAddTo(out, errw io.Writer, cfg config.Config, args []string) int {
	var name string
	opt := accounts.SeedOptions{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--share-config":
			// Bare: the primary's config, whitelisted. With a value: that dir,
			// every entry (a config package holds config and nothing else).
			opt.ShareFrom, opt.ShareNames = cfg.PrimaryDir(), accounts.SharedConfigEntries
		case strings.HasPrefix(a, "--share-config="):
			v := strings.TrimPrefix(a, "--share-config=")
			if v == "" {
				fmt.Fprintln(errw, "headroom accounts add: --share-config= needs a directory")
				return 2
			}
			if !filepath.IsAbs(v) {
				if abs, err := filepath.Abs(v); err == nil {
					v = abs
				}
			}
			opt.ShareFrom, opt.ShareNames = v, nil
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "headroom accounts add: unknown flag %q\n", a)
			return 2
		case name != "":
			fmt.Fprintf(errw, "headroom accounts add: unexpected argument %q\n", a)
			return 2
		default:
			name = a
		}
	}
	if name == "" {
		fmt.Fprintln(errw, "usage: headroom accounts add <email> [--share-config[=<dir>]]")
		return 2
	}
	dir, shared, err := accounts.Seed(cfg, name, opt)
	if err != nil {
		fmt.Fprintf(errw, "headroom accounts add: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "seeded %s\n", dir)
	fmt.Fprintf(out, "  projects → %s (sessions are machine-global)\n", cfg.ProjectsDir())
	if opt.ShareFrom != "" {
		if len(shared) == 0 {
			fmt.Fprintf(out, "  shared nothing from %s (no matching entries)\n", opt.ShareFrom)
		} else {
			fmt.Fprintf(out, "  shared from %s: %s\n", opt.ShareFrom, strings.Join(shared, ", "))
		}
	}
	fmt.Fprintf(out, "next: headroom launch --account %s   then /login as %s\n", name, name)
	fmt.Fprintf(out, "      (the board warns if the login does not match the dir name; new accounts sort last until listed in %s)\n", cfg.OrderFile())
	return 0
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

func runAccountsRemove(cfg config.Config, args []string) int {
	return runAccountsRemoveTo(os.Stdout, os.Stderr, cfg, args, removeDeps{
		probe:          psProbe,
		deleteKeychain: creds.DeleteKeychainItem,
		stdin:          os.Stdin,
		interactive:    term.IsTerminal(int(os.Stdin.Fd())),
	})
}

func runAccountsRemoveTo(out, errw io.Writer, cfg config.Config, args []string, deps removeDeps) int {
	var name string
	yes := false
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "headroom accounts remove: unknown flag %q\n", a)
			return 2
		case name != "":
			fmt.Fprintf(errw, "headroom accounts remove: unexpected argument %q\n", a)
			return 2
		default:
			name = a
		}
	}
	if name == "" {
		fmt.Fprintln(errw, "usage: headroom accounts remove <email | name.lock> [--yes]")
		return 2
	}
	// The primary is not removable: it is Claude Code's default dir, not an
	// entry in the accounts root, and its name is not a dir name at all.
	accts := accounts.Discover(cfg)
	var target *accounts.Account
	for i := range accts {
		if accts[i].Name == name {
			target = &accts[i]
			break
		}
	}
	if target != nil && target.IsPrimary() {
		fmt.Fprintf(errw, "headroom accounts remove: %q is the primary ~/.claude — Claude Code's own default dir is never removed by headroom\n", name)
		return 1
	}
	dir := filepath.Join(cfg.AccountsRoot, name)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(errw, "headroom accounts remove: %s is not an account dir\n", dir)
		return 1
	}

	// Gate: a session this dir is driving right now. Same rule as the
	// picker's dd — verified live refuses, and so does "could not verify":
	// deleting a config dir under a running claude orphans its login state.
	entries := sessions.ReadRegistry(name, dir)
	for id, st := range sessions.Liveness(entries, deps.probe) {
		switch st {
		case sessions.Live:
			fmt.Fprintf(errw, "headroom accounts remove: %s has a live session (%s) — quit it first\n", name, id)
			return 1
		case sessions.LiveUnknown:
			fmt.Fprintf(errw, "headroom accounts remove: %s has a registered session (%s) whose liveness could not be verified — quit any claude on this account and retry\n", name, id)
			return 1
		}
	}

	login := ""
	if target != nil {
		login = target.Email
	}
	if !yes {
		if !deps.interactive {
			fmt.Fprintln(errw, "headroom accounts remove: refusing without confirmation off a terminal — pass --yes")
			return 2
		}
		if login != "" {
			fmt.Fprintf(out, "removing %s (logged in as %s)\n", dir, login)
		} else {
			fmt.Fprintf(out, "removing %s\n", dir)
		}
		fmt.Fprintf(out, "type the dir name to confirm: ")
		reply, _ := bufio.NewReader(deps.stdin).ReadString('\n')
		if strings.TrimSpace(reply) != name {
			fmt.Fprintln(errw, "aborted")
			return 1
		}
	}

	// Keychain before dir: the service name is derived from the dir's
	// spelling, not its existence, so either order works — but if the
	// deletion fails, the dir is still there to retry against.
	deleted, err := deps.deleteKeychain(dir)
	switch {
	case err != nil:
		fmt.Fprintf(errw, "headroom accounts remove: %v — dir left in place\n", err)
		return 1
	case deleted:
		fmt.Fprintf(out, "deleted Keychain item (%s)\n", creds.ServiceName(dir))
	default:
		fmt.Fprintf(out, "no Keychain item (%s) — never logged in, or already gone\n", creds.ServiceName(dir))
	}
	if err := accounts.RemoveDir(cfg, name); err != nil {
		fmt.Fprintf(errw, "headroom accounts remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "removed %s\n", dir)
	fmt.Fprintln(out, "transcripts are machine-global and survive; the session picker shows this owner as degraded until each is re-homed")
	if cur, err := os.ReadFile(cfg.CurrentFile()); err == nil && strings.TrimSpace(string(cur)) == name {
		fmt.Fprintln(out, "note: .current pointed here — launches refuse until `headroom accounts` repicks")
	}
	return 0
}
