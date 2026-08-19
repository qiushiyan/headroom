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
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tui"
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
	// pick chooses among the removable candidates when the command was
	// given no name, and confirms the choice; ok=false is a cancel. nil
	// means the terminal picker.
	pick func(cands []removeCandidate) (name string, ok bool)
}

func runAccountsRemove(cfg config.Config, args []string) int {
	return runAccountsRemoveTo(os.Stdout, os.Stderr, cfg, args, removeDeps{
		probe:          psProbe,
		deleteKeychain: creds.DeleteKeychainItem,
		stdin:          os.Stdin,
		// Both ends, as the board requires: the picker draws on stdout and
		// the y/N prompt is printed there, so a redirected stdout with a
		// tty stdin would wait on a question nobody can see.
		interactive: term.IsTerminal(int(os.Stdin.Fd())) && stdoutIsTTY(),
	})
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
// dirs by design, so they are gathered here from the root directly. An
// accounts root that does not exist is an empty list; one that cannot be
// read is returned as the error, so "nothing to remove" is never said over
// a root that was merely unreadable.
func removeCandidates(cfg config.Config) ([]removeCandidate, error) {
	var cands []removeCandidate
	for _, a := range accounts.Discover(cfg) {
		if a.IsPrimary() {
			continue
		}
		cands = append(cands, removeCandidate{Name: a.Name, Email: a.Email})
	}
	entries, err := os.ReadDir(cfg.AccountsRoot)
	if err != nil && !os.IsNotExist(err) {
		return cands, fmt.Errorf("listing %s: %w", cfg.AccountsRoot, err)
	}
	for _, e := range entries {
		if accounts.LockArtifact(e.Name()) && e.IsDir() {
			cands = append(cands, removeCandidate{Name: e.Name(), Lock: true})
		}
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

// pickRemovable is the terminal picker behind a bare `headroom accounts
// remove`: the candidates as a list, up/down/enter, any cancel key aborts.
// Enter asks "remove X?" as one more keypress *inside the same raw session*
// — the cooked-mode line prompt cannot follow a closed tui session, because
// the terminal's reader goroutine stays blocked on stdin and would swallow
// the reply. In that second step Enter (or y) confirms and esc/n/q cancel:
// the row was already chosen with Enter, so the second Enter is an explicit
// re-affirmation, not the trap "any other key cancels" made of it; cancel
// stays one key away and an unrelated key is ignored rather than read as
// either answer. ok=true means the user has confirmed. The frame draws in
// place on stdout like the board and stays on screen.
func pickRemovable(cands []removeCandidate) (string, bool) {
	t, err := tui.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom accounts remove: %v\n", err)
		return "", false
	}
	defer t.Close()
	p := render.NewPalette(true)
	fp := &framePrinter{}
	t.OnClose(fp.finish)
	sel := 0
	width := 0
	for _, c := range cands {
		if n := len(c.Name); n > width {
			width = n
		}
	}
	confirming := false
	draw := func() {
		var lines []string
		if confirming {
			lines = append(lines, p.Bold+"remove "+cands[sel].Name+"?"+p.Rst+p.Dim+"  enter/y confirm · esc/n cancel"+p.Rst)
		} else {
			lines = append(lines, p.Bold+"remove which account?"+p.Rst+p.Dim+"  ↑/↓ move · enter choose · esc cancel"+p.Rst)
		}
		for i, c := range cands {
			prefix := "  "
			if i == sel {
				prefix = p.Bold + "▶ " + p.Rst
			}
			lines = append(lines, fmt.Sprintf("%s%-*s  %s%s%s", prefix, width, c.Name, p.Dim, c.describe(), p.Rst))
		}
		if confirming {
			lines = append(lines, p.Dim+"deletes its dir and Keychain item; transcripts survive"+p.Rst)
		}
		w, h := fp.geometry()
		fp.print(lines, w, h)
	}
	draw()
	for k := range t.Events() {
		if confirming {
			switch {
			case k.Kind == tui.KeyEnter, k == tui.Key{Kind: tui.KeyRune, Rune: 'y'}, k == tui.Key{Kind: tui.KeyRune, Rune: 'Y'}:
				return cands[sel].Name, true
			case isCancelKey(k), k == tui.Key{Kind: tui.KeyRune, Rune: 'n'}, k == tui.Key{Kind: tui.KeyRune, Rune: 'N'}:
				return "", false
			}
			continue
		}
		switch {
		case k.Kind == tui.KeyUp || k == tui.Key{Kind: tui.KeyRune, Rune: 'k'}:
			if sel > 0 {
				sel--
			}
		case k.Kind == tui.KeyDown || k == tui.Key{Kind: tui.KeyRune, Rune: 'j'}:
			if sel < len(cands)-1 {
				sel++
			}
		case isCancelKey(k):
			return "", false
		case k.Kind == tui.KeyEnter:
			confirming = true
		}
		draw()
	}
	return "", false
}

func listRemovable(w io.Writer, cands []removeCandidate, err error) {
	if err != nil {
		fmt.Fprintf(w, "headroom accounts remove: %v\n", err)
		return
	}
	if len(cands) == 0 {
		fmt.Fprintln(w, "nothing to remove: no extra accounts under the accounts root")
		return
	}
	fmt.Fprintln(w, "removable:")
	for _, c := range cands {
		fmt.Fprintf(w, "  %s  (%s)\n", c.Name, c.describe())
	}
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
		// Bare invocation on a terminal: choose from what is removable.
		// Off a terminal there is nobody to choose, so it is a usage error
		// that at least names the choices.
		cands, err := removeCandidates(cfg)
		if !deps.interactive || len(cands) == 0 || err != nil {
			fmt.Fprintln(errw, "usage: headroom accounts remove [<email | name.lock>] [--yes]")
			listRemovable(errw, cands, err)
			return 2
		}
		pick := deps.pick
		if pick == nil {
			pick = pickRemovable
		}
		chosen, ok := pick(cands)
		if !ok {
			fmt.Fprintln(errw, "aborted")
			return 1
		}
		name = chosen
		// The picker confirmed; from here the path is --yes's: every
		// refusal, then the gate once, immediately before the Keychain step.
		yes = true
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
	// A name nothing answers to — a typo, or an account already gone — gets
	// the list of what would have worked, not just a missing-path error.
	if _, err := os.Lstat(dir); err != nil && os.IsNotExist(err) {
		fmt.Fprintf(errw, "headroom accounts remove: no account named %q\n", name)
		cands, err := removeCandidates(cfg)
		listRemovable(errw, cands, err)
		return 1
	}
	// Every filesystem refusal RemoveDir will make, before anything else —
	// including the one that guards data: a real projects/ directory holds
	// sessions never migrated into the store.
	if err := accounts.CheckRemovable(cfg, name); err != nil {
		fmt.Fprintf(errw, "headroom accounts remove: %v\n", err)
		return 1
	}

	// Gate: a session this dir is driving right now. Same rule as the
	// picker's dd — verified live refuses, and so does "could not verify":
	// deleting a config dir under a running claude orphans its login state.
	// Strict read: a registry file that does not parse, or a sessions/ dir
	// that cannot be listed, is "could not verify", not "nothing there".
	gate := func() bool {
		entries, err := sessions.ReadRegistryStrict(name, dir)
		if err != nil {
			fmt.Fprintf(errw, "headroom accounts remove: %s: live-session registry unreadable (%v) — refusing while liveness cannot be verified\n", name, err)
			return false
		}
		for id, st := range sessions.Liveness(entries, deps.probe) {
			switch st {
			case sessions.Live:
				fmt.Fprintf(errw, "headroom accounts remove: %s has a live session (%s) — quit it first\n", name, id)
				return false
			case sessions.LiveUnknown:
				fmt.Fprintf(errw, "headroom accounts remove: %s has a registered session (%s) whose liveness could not be verified — quit any claude on this account and retry\n", name, id)
				return false
			}
		}
		return true
	}
	if !gate() {
		return 1
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
			fmt.Fprintln(errw, "aborted")
			return 1
		}
	}

	// The prompt may have stayed open for minutes; a session started
	// meanwhile must refuse the same way. The gate runs again immediately
	// before the first irreversible step.
	if !yes && !gate() {
		return 1
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
