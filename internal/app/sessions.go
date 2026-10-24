package app

// The `headroom sessions` command: its argument contract, the collection the
// picker and --json share, and the process edges both reach through. The
// picker's selection, modes and drawing are sessions_picker.go; the actions a
// commit runs are session_actions.go; the machine listing is
// sessions_json.go.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tui"
)

// execSessions is the exec edge, injected for tests. ExecPath, not Exec: the
// executable is resolved before the chdir, so a relative PATH entry in the
// project directory can never supply the binary.
var execSessions = launch.ExecPath

// psProbe keeps absent processes distinct from failed inspection.
func psProbe(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 && len(exit.Stderr) == 0 {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// accountRefs is the set as the session collector names accounts: each one's
// name and the config dir its history and registry live in.
func accountRefs(set accounts.Set) []sessions.AccountRef {
	refs := make([]sessions.AccountRef, 0, len(set.Accounts))
	for _, a := range set.Accounts {
		refs = append(refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir()})
	}
	return refs
}

func collectSessions(cfg config.Scope, st *state.Store) (sessions.Listing, accounts.Set, accounts.Routing) {
	set := accounts.Discover(cfg)
	cwd, _ := os.Getwd()
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.StoreDir(),
		CWD:         cwd,
		Accounts:    accountRefs(set),
		Owners:      st.Load().Owners(),
		Probe:       psProbe,
	})
	// Strict, exactly as launch resolves it: corrupt or dangling `.current`
	// yields "" — no current — rather than a primary nobody chose. Rows fall
	// back to the current account by doctrine, so a tolerant read here would
	// be the resume surface's route around the launch path's refusal.
	// One read serves both facts: the current account, and whether there is
	// none because launches choose.
	return listing, set, set.Routing()
}

func runSessions(cfg config.Scope, args []string) int {
	jsonMode := false
	cdFile := ""
	var claudeArgs []string
parse:
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonMode = true
		case "--cd-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom sessions: --cd-file needs a path")
				return 2
			}
			i++
			cdFile = args[i]
		case "--":
			claudeArgs = args[i+1:]
			break parse
		default:
			fmt.Fprintf(os.Stderr, "headroom sessions: unknown argument %q (claude args go after --)\n", args[i])
			return 2
		}
	}
	if jsonMode {
		if cdFile != "" || len(claudeArgs) > 0 {
			fmt.Fprintln(os.Stderr, "headroom sessions: --json takes no other arguments")
			return 2
		}
		return runSessionsJSON(cfg)
	}
	// This surface's contract is that the picker chooses the session; a
	// pass-through flag that chooses one contradicts it regardless of how
	// the vendor would rank the two.
	for _, a := range claudeArgs {
		if a == "--resume" || a == "-r" || a == "--continue" || a == "-c" {
			fmt.Fprintf(os.Stderr, "headroom sessions: %q cannot pass through — the picker chooses the session\n", a)
			return 2
		}
	}
	if cdFile != "" {
		// Absolute, same rule as every path here; created (or truncated) now,
		// before any side effect, so absence-of-content is observable even
		// through a wrapper that wrongly reuses a fixed path: empty means "no
		// launch was committed", non-empty means the dir was entered.
		if !filepath.IsAbs(cdFile) {
			fmt.Fprintf(os.Stderr, "headroom sessions: --cd-file %q is not absolute\n", cdFile)
			return 2
		}
		f, err := os.OpenFile(cdFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom sessions: --cd-file: %v\n", err)
			return 2
		}
		f.Close()
	}
	// Enter execs claude with the fds this process holds. A captured stdout
	// (`$(headroom sessions)`) would hand claude a pipe and hang the session
	// — the successor of the old line-protocol capture, refused up front.
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "headroom sessions: execs claude in this terminal — run it without capturing stdin or stdout")
		return 1
	}

	st := state.Open(cfg)
	listing, set, routing := collectSessions(cfg, st)
	t, err := tui.OpenTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom sessions: %v\n", err)
		return 1
	}
	defer t.Close()

	ui := &resumeUI{t: t, p: render.NewPalette(true), listing: listing,
		sessionActions: sessionActions{st: st, set: set, routing: routing, cdFile: cdFile, claudeArgs: claudeArgs, beforeLaunch: t.Close}}
	if !st.Load().OwnersReadable() {
		// Routing has silently fallen back to derived evidence. Degraded
		// attribution is supposed to be visible, and this is the only moment
		// the user can be told that a re-home they made is not being applied.
		ui.message = "re-home records unreadable — routing by vendor evidence only (headroom check)"
	}
	return ui.run()
}
