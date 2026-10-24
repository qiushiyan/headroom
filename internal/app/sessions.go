package app

// The `headroom sessions` command: its contract with the shell wrapper and
// the collection the picker and --json share. The picker's selection, modes
// and drawing are sessions_picker.go; the actions a commit runs are
// session_actions.go; the machine listing is sessions_json.go.

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tui"
)

func collectSessions(cfg config.Scope, st *state.Store, probe sessions.PIDProbe) (sessions.Listing, accounts.Set, accounts.Routing) {
	set := accounts.Discover(cfg)
	cwd, _ := os.Getwd()
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.StoreDir(),
		CWD:         cwd,
		Accounts:    launchop.Refs(set),
		Owners:      st.Load().Owners(),
		Probe:       probe,
	})
	// Strict, exactly as launch resolves it: corrupt or dangling `.current`
	// yields "" — no current — rather than a primary nobody chose. Rows fall
	// back to the current account by doctrine, so a tolerant read here would
	// be the resume surface's route around the launch path's refusal.
	// One read serves both facts: the current account, and whether there is
	// none because launches choose.
	return listing, set, set.Routing()
}

// SessionsOptions are what the shell wrapper and a person hand the session
// picker.
type SessionsOptions struct {
	// JSON lists the sessions instead of picking one; nothing else applies.
	JSON bool
	// CDFile is an absolute path the picker records the entered project dir
	// in, for the shell's own cd after the session ends: created empty
	// before anything else, written only when a launch is committed.
	CDFile string
	// Args reach claude exactly as given, before the picker's --resume.
	Args []string
}

// SessionsDeps are the edges the session picker reaches through: a launch's,
// and the exec that replaces this process with claude.
type SessionsDeps struct {
	Place launchop.Deps
	Exec  ExecFunc
}

// Sessions is the session picker: every Claude Code session on this machine,
// entered in its own project dir and continued, by exec, on the account that
// last drove it — or, with JSON, the listing as a document.
func Sessions(std Streams, term Terminal, scope config.Scope, opts SessionsOptions, deps SessionsDeps) error {
	if opts.JSON {
		return sessionsJSON(std, scope, deps.Place.Probe)
	}
	// This surface's contract is that the picker chooses the session; a
	// pass-through flag that chooses one contradicts it regardless of how
	// the vendor would rank the two.
	for _, a := range opts.Args {
		if a == "--resume" || a == "-r" || a == "--continue" || a == "-c" {
			return usagef("%q cannot pass through — the picker chooses the session", a)
		}
	}
	if opts.CDFile != "" {
		// Absolute, same rule as every path here; created (or truncated) now,
		// before any side effect, so absence-of-content is observable even
		// through a wrapper that wrongly reuses a fixed path: empty means "no
		// launch was committed", non-empty means the dir was entered.
		if !filepath.IsAbs(opts.CDFile) {
			return usagef("--cd-file %q is not absolute", opts.CDFile)
		}
		f, err := os.OpenFile(opts.CDFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return usagef("--cd-file: %v", err)
		}
		f.Close()
	}
	// Enter execs claude with the fds this process holds. A captured stdout
	// (`$(headroom sessions)`) would hand claude a pipe and hang the session
	// — the successor of the old line-protocol capture, refused up front.
	if !term.In || !term.Out {
		return errors.New("execs claude in this terminal — run it without capturing stdin or stdout")
	}

	st := state.Open(scope)
	listing, set, routing := collectSessions(scope, st, deps.Place.Probe)
	t, err := tui.OpenTTY()
	if err != nil {
		return err
	}
	defer t.Close()

	ui := &resumeUI{t: t, p: render.NewPalette(true), listing: listing,
		st: st, set: set, routing: routing, cdFile: opts.CDFile, claudeArgs: opts.Args, beforeLaunch: t.Close,
		errw: std.Err, place: deps.Place, exec: deps.Exec}
	if !st.Load().OwnersReadable() {
		// Routing has silently fallen back to derived evidence. Degraded
		// attribution is supposed to be visible, and this is the only moment
		// the user can be told that a re-home they made is not being applied.
		ui.message = "re-home records unreadable — routing by vendor evidence only (headroom check)"
	}
	if code := ui.run(); code != 0 {
		if ui.failed != nil {
			return ui.failed
		}
		return Status(code)
	}
	return nil
}
