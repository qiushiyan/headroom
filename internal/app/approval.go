package app

// A vendor command run with headroom as its $BROWSER: `headroom login`'s
// `claude auth login` and `headroom headless mint`'s `claude setup-token`.
// Both open one approval page, which a person approves as one account, so
// where that page goes, what the person is told about it, and the
// environment that makes headroom the vendor's opener live here, once.
// What the two vendor commands force apart stays with each command: `auth
// login` reads its code from a pipe headroom feeds (relayCodes) and draws on
// the terminal, while `setup-token` must have the terminal on its input and
// is read off a pipe (headless.Scanner). See DESIGN.md § Renewing logins.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
)

// approver decides where approval pages go on this machine: the Chrome
// profiles it has, and whether the user is at its screen.
type approver struct {
	profiles []browser.Profile
	// unread is why no profile list was read, with the path it was read
	// from: a plan then names that rather than a failed match.
	unread error
	// remote: the user is not at this machine's screen, so a page opened
	// here is a page nobody sees.
	remote bool
}

func newApprover(scope config.Scope) approver {
	path := browser.LocalStatePath(scope.Home)
	profiles, err := browser.ReadProfiles(path)
	ap := approver{profiles: profiles, remote: sshRemote(os.Getenv("SSH_CONNECTION"))}
	if err != nil {
		ap.unread = profileListError{path: path, err: err}
	}
	return ap
}

// profileListError is why Chrome's profile list gave no profiles.
type profileListError struct {
	path string
	err  error
}

func (e profileListError) Error() string {
	if errors.Is(e.err, fs.ErrNotExist) {
		return "no Chrome profile list at " + e.path
	}
	why := e.err
	if pe, ok := errors.AsType[*fs.PathError](e.err); ok {
		why = pe.Err // the path is said once
	}
	return "Chrome's profile list at " + e.path + " could not be read: " + why.Error()
}

// sshRemote reports whether SSH_CONNECTION ("client port server port") puts
// the user on another machine. An ssh from a machine to itself — how the
// author's mini attaches its own tmux — leaves the user at this screen.
func sshRemote(conn string) bool {
	f := strings.Fields(conn)
	if len(f) < 3 {
		return false
	}
	client, server := f[0], f[2]
	return client != server && !strings.HasPrefix(client, "127.") && client != "::1"
}

// approvalPage is where one vendor approval page goes, for the account it
// must be approved as: the Chrome profile matched to that email, the default
// browser when none matches, or, for a remote user, nowhere — they open the
// printed URL where they are and paste back the code.
type approvalPage struct {
	email   string // whom the page must be approved as; "" = nobody known
	remote  bool
	profile browser.Profile // the matched profile, when matched
	matched bool
	unread  error // why no profile could be matched at all, when none was read
}

// page is where the approval for email goes.
func (ap approver) page(email string) approvalPage {
	pg := approvalPage{email: email, remote: ap.remote}
	if !ap.remote {
		pg.profile, pg.matched = browser.Match(ap.profiles, email)
		pg.unread = ap.unread
	}
	return pg
}

// phrase is where the page opens, in words, for a plan line.
func (pg approvalPage) phrase() string {
	switch {
	case pg.remote:
		return "over ssh: you open the printed URL where you are"
	case pg.matched:
		return "Chrome profile " + pg.profile.Label()
	case pg.unread != nil:
		return "default browser (" + pg.unread.Error() + ")"
	}
	return "default browser (no Chrome profile matches " + orNobody(pg.email) + ")"
}

// remoteSteps is what a remote user does instead of clicking in a page that
// opened: the vendor prints the URL, and its page shows the code.
func (pg approvalPage) remoteSteps() string {
	return "open the URL below in a browser signed in to claude.ai as " + orNobody(pg.email) + ",\n" +
		"click Authorize, and paste the code it shows"
}

// recorded reports whether headroom is the vendor's $BROWSER for this page,
// and so records which sign-in the vendor started: on macOS, where a profile
// is opened with open(1), and for a remote user, for whom it opens nothing.
// Elsewhere, at the machine, the vendor's own browser handling stays.
func (pg approvalPage) recorded() bool { return pg.remote || runtime.GOOS == "darwin" }

// env is base with the opener's variables set for this page: headroom as
// $BROWSER, recording the sign-in's state at signin and, for a user at the
// machine, opening the page in its profile ("" = the default browser). The
// opener's variables are headroom's to set and are never inherited; the
// user's own $BROWSER is replaced only where headroom opens the page.
func (pg approvalPage) env(base []string, signin string) ([]string, error) {
	env := withoutVars(base, browser.ProfileEnv, browser.SigninEnv)
	if !pg.recorded() {
		return env, nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("no browser command: %w", err)
	}
	env = append(withoutVars(env, "BROWSER"), "BROWSER="+self, browser.SigninEnv+"="+signin)
	if !pg.remote {
		env = append(env, browser.ProfileEnv+"="+pg.profile.Dir)
	}
	return env, nil
}

func withoutVars(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// runOpen is the whole of headroom as a vendor's $BROWSER: record in the
// file signin names which sign-in url belongs to, show url in the profile
// the page was matched to when open, and nothing else.
func runOpen(errw io.Writer, url, signin, profileDir string, open bool) int {
	if signin != "" {
		// Best effort: an unrecorded sign-in leaves its code unchecked, never
		// its page unopened.
		recordSignin(signin, browser.SigninState(url))
	}
	if !open {
		return 0
	}
	out, err := exec.Command("open", browser.OpenArgs(profileDir, url)...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || len(out) > 0 {
			fmt.Fprintf(errw, "headroom: open: %s\n", strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(errw, "headroom: open: %v\n", err)
		}
		return 1
	}
	return 0
}

// recordSignin writes state whole, by rename: a code pasted while it is
// written is never checked against part of it.
func recordSignin(path, state string) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(state), 0o600) == nil {
		os.Rename(tmp, path)
	}
}
