package app

// `headroom login`: renew the Claude Code logins that end soon, in one pass.
// The login itself is Claude Code's — `claude auth login`, run under the
// account's environment — so headroom still writes no credential. What
// headroom adds is the choice of accounts, the browser profile the approval
// page opens in, and the read-back that says whether each login took. Spec:
// docs/specs/login.md.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/tag"
)

// defaultLoginWithin is how close a login's end must be for a bare
// `headroom login` to renew it. Claude Code warns at three days; a week
// leaves room to renew a batch together, which is the point.
const defaultLoginWithin = 7 * 24 * time.Hour

// loginDeps are the edges `headroom login` crosses — the health probe, the
// credential and identity reads, the Keychain's reach, the browser registry,
// where the user is, and the vendor's login process — injected so selection
// and read-back are testable without a Keychain, a browser or a network.
type loginDeps struct {
	health    func([]accounts.Account) auth.QueryFunc
	readRaw   func(accounts.Account) string
	metaEmail func(accounts.Account) string
	// sealed: the login Keychain refuses this session, as macOS refuses an
	// ssh session. inKeychain reports an account's item by its attributes,
	// which a sealed Keychain still shows.
	sealed     bool
	inKeychain func(accounts.Account) bool
	profiles   []browser.Profile
	// remote: the user is not at this machine's screen, so a page opened
	// here is a page nobody sees.
	remote bool
	now    func() time.Time
	login  func(acct accounts.Account, set accounts.Set, email string, page approvalPage) error
}

// approvalPage is where the vendor's approval page goes: the Chrome profile
// at profileDir ("" = the default browser), or, for a remote user, nowhere —
// they open the printed URL where they are and paste back the code.
type approvalPage struct {
	profileDir string
	remote     bool
}

// loginRow is one account as `login` sees it before acting.
type loginRow struct {
	acct   accounts.Account
	email  string // the address the login is for; "" for a primary never logged in
	health accountstate.Health
	blob   creds.Blob
	blobOK bool
	// sealed: the login lives in a Keychain this session cannot read, so its
	// state is unknown here and a login made here would land in a file that
	// any session able to read the Keychain ignores.
	sealed bool
	chosen bool
	why    string // why chosen, or why left alone
}

func runLogin(scope config.Scope, args []string) int {
	return runLoginTo(os.Stdout, os.Stderr, scope, args, loginDeps{
		health: queryHealthParallel,
		readRaw: func(a accounts.Account) string {
			raw, _ := creds.ReadRaw(a.ConfigDir, a.Dir())
			return raw
		},
		metaEmail: func(a accounts.Account) string {
			email, _ := accounts.MetaEmail(a.MetaPath())
			return email
		},
		sealed:     creds.KeychainSealed(),
		inKeychain: func(a accounts.Account) bool { return creds.HasKeychainItem(a.ConfigDir) },
		profiles:   browser.ReadProfiles(browser.LocalStatePath(scope.Home)),
		remote:     sshRemote(os.Getenv("SSH_CONNECTION")),
		now:        time.Now,
		login:      vendorLogin,
	})
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

func runLoginTo(out, errw io.Writer, scope config.Scope, args []string, deps loginDeps) int {
	var names []string
	all, dryRun := false, false
	within := defaultLoginWithin
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--all":
			all = true
		case a == "--dry-run":
			dryRun = true
		case a == "--within":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "headroom login: --within needs a number of days")
				return 2
			}
			i++
			days, err := strconv.Atoi(args[i])
			if err != nil || days < 0 {
				fmt.Fprintf(errw, "headroom login: --within %q is not a number of days\n", args[i])
				return 2
			}
			within = time.Duration(days) * 24 * time.Hour
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "headroom login: unknown flag %q\n", a)
			return 2
		default:
			names = append(names, a)
		}
	}
	if all && len(names) > 0 {
		fmt.Fprintln(errw, "headroom login: --all and account names exclude each other")
		return 2
	}

	set := accounts.Discover(scope)
	named := map[string]bool{}
	for _, n := range names {
		a, err := set.Select(n)
		if err != nil {
			fmt.Fprintf(errw, "headroom login: %v\n", err)
			return 1
		}
		named[a.Name] = true
	}

	now := deps.now()
	rows := assessLogins(set.Accounts, deps, now)
	chooseLogins(rows, named, all, within, now)

	var chosen []*loginRow
	sealed, refused := 0, 0
	for _, r := range rows {
		line := fmt.Sprintf("  %-28s %s", r.acct.Name, r.why)
		if r.chosen {
			chosen = append(chosen, r)
			line = "→" + line[1:] + " — " + pagePhrase(deps, r.email)
		}
		if r.sealed {
			sealed++
			if named[r.acct.Name] || all {
				refused++
			}
		}
		fmt.Fprintln(out, line)
	}
	if sealed > 0 {
		fmt.Fprintf(out, "\nthe login Keychain is closed to this ssh session: the %d accounts marked above keep their\n"+
			"login there, so it can be neither read nor renewed from here — a login made here would be\n"+
			"saved to a file that every session able to read the Keychain ignores. Run\n"+
			"`security unlock-keychain` in this session first (it asks for this Mac's password), or\n"+
			"run headroom login at the machine.\n", sealed)
	}
	if len(chosen) == 0 {
		if refused > 0 {
			return 1
		}
		if sealed == 0 {
			fmt.Fprintf(out, "nothing to renew within %s; name an account or pass --all to renew anyway\n", days(within))
		}
		return 0
	}
	if dryRun {
		return 0
	}

	failed := refused
	for i, r := range chosen {
		page := approvalPage{remote: deps.remote}
		if p, ok := browser.Match(deps.profiles, r.email); ok && !deps.remote {
			page.profileDir = p.Dir
		}
		if page.remote {
			fmt.Fprintf(out, "\n[%d/%d] %s — open the URL below in a browser signed in to claude.ai as %s,\n"+
				"click Authorize, and paste the code it shows\n", i+1, len(chosen), r.acct.Name, orNobody(r.email))
		} else {
			fmt.Fprintf(out, "\n[%d/%d] %s — click Authorize in the page that opens\n", i+1, len(chosen), r.acct.Name)
		}
		err := deps.login(r.acct, set, r.email, page)
		if msg := readBack(r, err, deps); msg != "" {
			failed++
			fmt.Fprintf(out, "✗ %s: %s\n", r.acct.Name, msg)
			continue
		}
		blob, _ := creds.Parse(deps.readRaw(r.acct))
		fmt.Fprintf(out, "✓ %s: logged in%s\n", r.acct.Name, endPhrase(blob, deps.now()))
	}
	if failed > 0 {
		fmt.Fprintf(out, "\n%d of %d logins did not take\n", failed, len(chosen)+refused)
		return 1
	}
	return 0
}

// assessLogins reads what the board reads about each account's access: the
// vendor's own verdict where it has one, credential evidence otherwise.
func assessLogins(accts []accounts.Account, deps loginDeps, now time.Time) []*loginRow {
	health := deps.health(accts)
	rows := make([]*loginRow, len(accts))
	for i, a := range accts {
		raw := deps.readRaw(a)
		blob, ok := creds.Parse(raw)
		rows[i] = &loginRow{
			acct:   a,
			email:  loginEmail(a),
			health: resolveHealth(health(a.ConfigDir), raw, blob, ok, now.UnixMilli()),
			blob:   blob,
			blobOK: ok,
			sealed: deps.sealed && deps.inKeychain(a),
		}
	}
	return rows
}

// loginEmail is the address an account's login is for. An extra's dir is
// named by the subscription it holds, so the name is the intent even when
// the dir is logged in as someone else; the primary has no such name and is
// whoever its identity document says.
func loginEmail(a accounts.Account) string {
	if a.IsPrimary() {
		return a.Email
	}
	return a.Name
}

// chooseLogins marks the accounts to renew. Names and --all choose outright;
// otherwise only positive evidence does — no login, an end already passed,
// or an end inside the window. An expiry that is absent or unreadable is
// never read as near, the same rule the board's health follows.
func chooseLogins(rows []*loginRow, named map[string]bool, all bool, within time.Duration, now time.Time) {
	for _, r := range rows {
		state := loginState(r, now)
		switch {
		case r.sealed:
			r.why = "login kept in the Keychain, which this session cannot read"
		case named[r.acct.Name]:
			r.chosen, r.why = true, "named; "+state
		case len(named) > 0:
			r.why = state
		case all:
			r.chosen, r.why = true, state
		case r.health == accountstate.HealthNoLogin, r.health == accountstate.HealthReloginRequired:
			r.chosen, r.why = true, state
		case r.health == accountstate.HealthOK && r.blobOK && r.blob.RefreshState == tag.OK &&
			time.UnixMilli(r.blob.RefreshExpiresMS).Sub(now) < within:
			r.chosen, r.why = true, state
		default:
			r.why = state
		}
	}
}

// loginState is one account's login in words, for the plan.
func loginState(r *loginRow, now time.Time) string {
	switch r.health {
	case accountstate.HealthNoLogin:
		return "not logged in"
	case accountstate.HealthReloginRequired:
		return "login ended " + when(r.blob.RefreshExpiresMS, now)
	case accountstate.HealthBadBlob:
		return "credential unreadable (Claude Code changed its format?)"
	case accountstate.HealthUnknown:
		return "login state unknown"
	}
	if !r.blobOK || r.blob.RefreshState != tag.OK {
		return "logged in, end date not recorded"
	}
	return "login ends " + when(r.blob.RefreshExpiresMS, now)
}

// readBack decides whether a login took: the stored credential must have
// changed, and the dir must now be logged in as the account it is for. ""
// means it took.
func readBack(r *loginRow, runErr error, deps loginDeps) string {
	blob, ok := creds.Parse(deps.readRaw(r.acct))
	changed := ok && (!r.blobOK || blob.Token != r.blob.Token || blob.RefreshExpiresMS != r.blob.RefreshExpiresMS)
	if !changed {
		if runErr != nil {
			return "claude auth login: " + runErr.Error()
		}
		return "the stored login did not change"
	}
	got := deps.metaEmail(r.acct)
	if r.email != "" && !strings.EqualFold(got, r.email) {
		return fmt.Sprintf("logged in as %s, not %s — run `headroom login %s` again and approve as %s", orNobody(got), r.email, r.acct.Name, r.email)
	}
	return ""
}

func pagePhrase(deps loginDeps, email string) string {
	if deps.remote {
		return "over ssh: you open the printed URL where you are"
	}
	if p, ok := browser.Match(deps.profiles, email); ok {
		return "Chrome profile " + p.Label()
	}
	return "default browser (no Chrome profile matches " + orNobody(email) + ")"
}

func endPhrase(b creds.Blob, now time.Time) string {
	if b.RefreshState != tag.OK {
		return ""
	}
	return ", login ends " + when(b.RefreshExpiresMS, now)
}

// when is an instant as a date plus its distance from now.
func when(ms int64, now time.Time) string {
	t := time.UnixMilli(ms)
	d := t.Sub(now)
	if d < 0 {
		return t.Local().Format("Jan 2") + " (" + days(-d) + " ago)"
	}
	return t.Local().Format("Jan 2") + " (in " + days(d) + ")"
}

func days(d time.Duration) string {
	if d < 24*time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

func orNobody(email string) string {
	if email == "" {
		return "nobody"
	}
	return email
}

// vendorLogin runs `claude auth login` for acct in the foreground, under the
// environment launch builds for it, with this binary as $BROWSER so the
// approval page opens in the matched profile — or, for a remote user,
// true(1), so nothing opens on a screen nobody is at. stdio is the
// terminal's: the vendor always prints the URL, and asks for the code there
// when the browser cannot reach its localhost callback.
func vendorLogin(acct accounts.Account, set accounts.Set, email string, page approvalPage) error {
	p, err := launch.Prepare(acct, set, os.Environ())
	if err != nil {
		return err
	}
	for _, n := range p.Notices {
		fmt.Fprintln(os.Stderr, "headroom: "+n)
	}
	opener, err := os.Executable()
	if page.remote {
		opener, err = exec.LookPath("true")
	}
	if err != nil {
		return fmt.Errorf("no browser command: %w", err)
	}
	args := []string{"auth", "login"}
	if email != "" {
		args = append(args, "--email", email)
	}
	cmd := exec.Command(p.Path, args...)
	cmd.Args[0] = p.Binary
	cmd.Env = append(withoutVars(p.Env, "BROWSER", browser.ProfileEnv), "BROWSER="+opener, browser.ProfileEnv+"="+page.profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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

// runOpen is the whole of headroom as a vendor's $BROWSER: show url in the
// profile `headroom login` chose, and nothing else.
func runOpen(profileDir, url string) int {
	out, err := exec.Command("open", browser.OpenArgs(profileDir, url)...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || len(out) > 0 {
			fmt.Fprintf(os.Stderr, "headroom: open: %s\n", strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(os.Stderr, "headroom: open: %v\n", err)
		}
		return 1
	}
	return 0
}
