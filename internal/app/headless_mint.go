package app

// `headroom headless mint <account>`: one long-lived token for one account,
// minted by the vendor's own `claude setup-token` and approved the way
// `headroom login` has a login approved — in the Chrome profile matched to
// the account, or, for a user on another machine, at the printed URL with
// the code pasted back. headroom keeps the token; the vendor keeps nothing.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

// mintDeps are the edges a mint crosses — the terminal, where approval
// pages go, and the vendor's command — injected so the command is testable
// without a browser or a vendor.
type mintDeps struct {
	terminal bool
	approver approver
	now      func() time.Time
	// setupToken runs the vendor's command, its output written to scan.
	setupToken func(scope config.Scope, page approvalPage, scan *headless.Scanner) error
}

// mintWait bounds how long a mint waits for the approval. The vendor ignores
// every key while it waits for one — ctrl-c included (2.1.296) — so without a
// bound, a mint nobody approves would hold its terminal until it is closed.
// mintGrace is how long the vendor is given to exit once it has printed the
// token: there is nothing left for it to do, since it stores nothing.
// Variables so a test can wait less.
var (
	mintWait  = 10 * time.Minute
	mintGrace = 5 * time.Second
)

func runHeadlessMint(scope config.Scope, args []string) int {
	return runHeadlessMintTo(os.Stdout, os.Stderr, scope, args, mintDeps{
		terminal:   term.IsTerminal(int(os.Stdin.Fd())),
		approver:   newApprover(scope),
		now:        time.Now,
		setupToken: vendorSetupToken,
	})
}

func runHeadlessMintTo(out, errw io.Writer, scope config.Scope, args []string, deps mintDeps) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(errw, "usage: headroom headless mint <account>")
		return 2
	}
	a, err := accounts.Discover(scope).Select(args[0])
	if err != nil {
		fmt.Fprintf(errw, "headroom headless mint: %v\n", err)
		return 1
	}
	// The token is counted against the subscription the dir is logged in to,
	// so that login must be there and must be the account's.
	switch {
	case !a.Readable:
		fmt.Fprintf(errw, "headroom headless mint: %s's login could not be read — try again, or run headroom check\n", a.Name)
		return 1
	case a.AccountID == "" || a.Email == "":
		fmt.Fprintf(errw, "headroom headless mint: %s is not logged in — headroom login %s first: a headless token is counted against the subscription its account's dir is logged in to\n", a.Name, a.Name)
		return 1
	case !a.IsPrimary() && !strings.EqualFold(a.Email, a.Name):
		fmt.Fprintf(errw, "headroom headless mint: %s's dir is logged in as %s — headroom login %s first\n", a.Name, a.Email, a.Name)
		return 1
	}
	if !deps.terminal {
		fmt.Fprintln(errw, "headroom headless mint: needs a terminal — the vendor asks for the approval there")
		return 1
	}
	// Before the approval, not after: a token minted into a file that cannot
	// take it is lost, since it is shown nowhere.
	if _, err := headless.Read(scope.AccountsRoot); err != nil {
		fmt.Fprintf(errw, "headroom headless mint: %v\n", err)
		return 1
	}
	key := state.KeyFor(a)

	page := deps.approver.page(a.Email)
	fmt.Fprintf(out, "minting a headless token for %s (claude setup-token)\n", a.Name)
	if page.remote {
		fmt.Fprintln(out, page.remoteSteps())
	} else {
		fmt.Fprintf(out, "approve as %s in %s — headroom cannot check which account approves\n", a.Email, page.phrase())
	}
	fmt.Fprintf(out, "it waits up to %s for the approval; ctrl-c does not reach it — close the terminal to stop sooner\n\n", render.Span(mintWait))

	scan := headless.NewScanner(out)
	runErr := deps.setupToken(scope, page, scan)
	_ = scan.Close()
	now := deps.now()
	value := scan.Token()
	if value == "" {
		why := "the vendor printed no token"
		if runErr != nil {
			why = "claude setup-token: " + runErr.Error()
		}
		fmt.Fprintf(out, "\n✗ %s: no headless token was minted (%s)\n", a.Name, why)
		return 1
	}
	tok := headless.Token{Value: value, MintedMS: now.UnixMilli(), Key: key.ID(), Email: a.Email}
	life, known := scan.Lifetime()
	if known {
		tok.EndsMS = now.Add(life).UnixMilli()
	}
	if err := headless.Put(scope.AccountsRoot, a.Name, tok); err != nil {
		fmt.Fprintf(out, "\n✗ %s: the token was minted but not stored (%v) — it was not shown; mint again\n", a.Name, err)
		return 1
	}
	ends := "the vendor did not say when it ends"
	if known {
		ends = "ends " + render.When(tok.EndsMS, now)
	}
	fmt.Fprintf(out, "\n✓ %s: headless token stored in %s, %s\n", a.Name, headless.Path(scope.AccountsRoot), ends)
	return 0
}

// vendorSetupToken runs `claude setup-token` with the terminal on its input,
// its output on scan, and page's opener as its $BROWSER — the vendor facts
// behind each, in DESIGN.md § Headless tokens.
//
// It runs under a config dir made for the run and removed after, so nothing
// in the account's dir, its login above all, is touched. The environment is
// launch's constructor applied to that dir, so no inherited CLAUDE_CONFIG_DIR
// or credential redirect reaches the vendor, less the inherited credentials,
// which would only have the vendor warn that one is configured. headroom
// ends the vendor, which ignores every key while it waits: after mintWait
// with no approval, or mintGrace after it has printed the token.
func vendorSetupToken(scope config.Scope, page approvalPage, scan *headless.Scanner) error {
	path, err := exec.LookPath(scope.Binary())
	if err != nil {
		return fmt.Errorf("%s not found on PATH: %w", scope.Binary(), err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "headroom-mint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	target, err := launch.Extra(config.Claude, scratch)
	if err != nil {
		return err
	}
	// The sign-in is recorded and never read: the code goes to the vendor's
	// own prompt. It is recorded so that, for a remote user, headroom is a
	// $BROWSER that opens nothing.
	env, err := page.env(withoutVars(target.Env(os.Environ()), "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"),
		filepath.Join(scratch, "headroom-signin"))
	if err != nil {
		return err
	}
	cmd := exec.Command(path, "setup-token")
	cmd.Args[0] = scope.Binary()
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, scan, scan

	// The vendor puts the terminal in raw mode for its prompt; whatever
	// becomes of it, the terminal is given back as it was. A signal headroom
	// is sent is passed on — a closed terminal's SIGHUP, a kill — and the
	// ctrl-c of a terminal not yet in raw mode has reached the vendor too.
	if st, err := term.GetState(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), st)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.NewTimer(mintWait)
	defer deadline.Stop()
	found := scan.Found()
	var grace <-chan time.Time
	var stopped error // why headroom ended it, when it did
	afterToken := false
	stop := func(why error) {
		stopped = why
		_ = cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(3 * time.Second)
			_ = cmd.Process.Kill() // a no-op once it has gone
		}()
	}
	for {
		select {
		case err := <-exited:
			switch {
			case afterToken:
				return nil // ended by headroom, with its work done
			case stopped != nil:
				return stopped
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() < 0 {
				return errors.New("stopped by a signal")
			}
			return err
		case s := <-sig:
			if s != syscall.SIGINT {
				_ = cmd.Process.Signal(s)
			}
		case <-found:
			found, grace = nil, time.After(mintGrace)
		case <-grace:
			grace, afterToken = nil, true
			stop(nil)
		case <-deadline.C:
			stop(fmt.Errorf("no approval within %s — stopped", render.Span(mintWait)))
		}
	}
}
