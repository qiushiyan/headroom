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
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tui"
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
	default:
		// Sharing is optional; what governs the shared session store is not.
		if names := accounts.StoreSettings(cfg.Vendor); names != nil {
			opt.ShareFrom, opt.ShareNames = cfg.PrimaryDir(), names
		}
	}
	dir, shared, kept, err := accounts.Seed(cfg, name, opt)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "seeded %s\n", dir)
	fmt.Fprintf(out, "  %s → %s (sessions are machine-global)\n", cfg.StoreLink(), cfg.StoreDir())
	if opt.ShareFrom != "" {
		if len(shared) == 0 {
			fmt.Fprintf(out, "  shared nothing from %s (no matching entries)\n", opt.ShareFrom)
		} else {
			fmt.Fprintf(out, "  shared from %s: %s\n", opt.ShareFrom, strings.Join(shared, ", "))
		}
		if len(kept) > 0 {
			// A login and its history are never shared, whatever the source
			// holds: this dir gets its own at its first login.
			fmt.Fprintf(out, "  kept per dir, not linked: %s\n", strings.Join(kept, ", "))
		}
	}
	// A second home's dir is logged in from that home: the launch must run
	// under its HOME, or it resolves the login home's accounts instead.
	under := ""
	if cfg.PrimaryExplicit {
		under = "HOME=" + cfg.Home + " "
	}
	if cfg.Vendor == config.Codex {
		// Codex has no in-session /login: logging in is one command, in the
		// engine's own spelling — a wrapper's argument passing is not
		// headroom's to know.
		fmt.Fprintf(out, "next: %sheadroom launch --vendor codex --account %s -- login   and log in as %s\n", under, name, name)
	} else {
		fmt.Fprintf(out, "next: %sheadroom launch --account %s   then /login as %s\n", under, name, name)
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
	// pick chooses among the removable candidates when the command was
	// given no name, and confirms the choice; ok=false is a cancel.
	pick func(cands []removeCandidate) (name string, ok bool)
}

// AccountsRemove removes one extra account: its Keychain item and its dir,
// refused while a session is live on it or its liveness cannot be verified,
// after a confirmation unless yes. A bare removal on a terminal picks from
// what is removable. It never touches .current. Codex accounts are never
// removed: headroom cannot tell whether a codex session is running on a home.
func AccountsRemove(std Streams, term Terminal, cfg config.Scope, name string, yes bool, probe sessions.PIDProbe) error {
	if cfg.Vendor == config.Codex {
		return codexRemoveRefusal(cfg, name)
	}
	return accountsRemove(std, cfg, name, yes, removeDeps{
		probe:          probe,
		deleteKeychain: creds.DeleteKeychainItem,
		stdin:          std.In,
		// Both ends, as the board requires: the picker draws on stdout and
		// the y/N prompt is printed there, so a redirected stdout with a
		// tty stdin would wait on a question nobody can see.
		interactive: term.In && term.Out,
		pick: func(cands []removeCandidate) (string, bool) {
			return pickRemovable(std, term, cands)
		},
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
func pickRemovable(std Streams, term Terminal, cands []removeCandidate) (string, bool) {
	t, err := tui.Open()
	if err != nil {
		fmt.Fprintf(std.Err, "headroom accounts remove: %v\n", err)
		return "", false
	}
	defer t.Close()
	p := render.NewPalette(true)
	fp := &framePrinter{out: std.Out, size: term.Size}
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
		// Bare invocation on a terminal: choose from what is removable.
		// Off a terminal there is nobody to choose, so it is a usage error
		// that at least names the choices.
		cands, err := removeCandidates(cfg)
		if !deps.interactive || len(cands) == 0 || err != nil {
			return usagef("name the account to remove — %s", removable(cands, err))
		}
		chosen, ok := deps.pick(cands)
		if !ok {
			return errors.New("aborted")
		}
		name = chosen
		// The picker confirmed; from here the path is --yes's: every
		// refusal, then the gate once, immediately before the Keychain step.
		yes = true
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

	// Gate: a session this dir is driving right now. Same rule as the
	// picker's dd — verified live refuses, and so does "could not verify":
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
	// A secret for an account that no longer exists is no use to anyone.
	// (Only Claude Code accounts reach here: Codex removal refuses up front.)
	if forgot, err := headless.Remove(cfg.AccountsRoot, name); err != nil {
		fmt.Fprintf(std.Err, "headroom accounts remove: its headless token was not forgotten (%v) — headroom headless forget %s\n", err, name)
	} else if forgot {
		fmt.Fprintln(out, "forgot its headless token — headroom does not revoke it with the vendor")
	}
	if err := accounts.SetReservation(cfg, name, nil); err != nil {
		fmt.Fprintf(std.Err, "headroom accounts remove: its reservation was not dropped (%v) — headroom accounts reserve %s --clear\n", err, name)
	}
	fmt.Fprintln(out, "transcripts are machine-global and survive; the session picker shows this owner as degraded until each is re-homed")
	if cur, err := os.ReadFile(cfg.CurrentFile()); err == nil && strings.TrimSpace(string(cur)) == name {
		fmt.Fprintln(out, "note: .current pointed here — launches refuse until `headroom accounts` repicks")
	}
	return nil
}
