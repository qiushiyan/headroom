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
	"runtime"
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
	// held is why this account cannot be renewed from here, even by name:
	// its login lives in a Keychain this session cannot read (sealed — its
	// state is unknown here, and a login made here would land in a file that
	// any session able to read the Keychain ignores), or nothing says whom
	// its login belongs to, so a new one could not be checked.
	held   string
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
		case a == "":
			// A caller's expansion bug (`headroom login "$name"`), never a
			// request for the pinned account, which is what Select reads "" as.
			fmt.Fprintln(errw, "headroom login: an account name cannot be empty")
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
		}
		if r.held != "" && (named[r.acct.Name] || all) {
			refused++
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
	var retry []string // names whose login ran and did not take
	var earliest int64 // the soonest end among the logins that took
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
		// The baseline is taken now, not when the batch was planned: a live
		// session may have refreshed this account while earlier approvals
		// were pending.
		before, beforeOK := creds.Parse(deps.readRaw(r.acct))
		err := deps.login(r.acct, set, r.email, page)
		blob, msg := readBack(r, before, beforeOK, err, deps)
		if msg != "" {
			failed++
			retry = append(retry, r.acct.Name)
			fmt.Fprintf(out, "✗ %s: %s\n", r.acct.Name, msg)
			continue
		}
		if blob.RefreshState == tag.OK && (earliest == 0 || blob.RefreshExpiresMS < earliest) {
			earliest = blob.RefreshExpiresMS
		}
		fmt.Fprintf(out, "✓ %s: logged in%s\n", r.acct.Name, endPhrase(blob, deps.now()))
	}
	// The batch's one date: when the next pass is due on this machine.
	if earliest != 0 {
		fmt.Fprintf(out, "\nthe logins renewed here end from %s\n", when(earliest, deps.now()))
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d of %d logins did not take\n", failed, len(chosen)+refused)
		if len(retry) > 0 {
			// Only the logins that ran and failed: retrying a held account is
			// refused again until what holds it changes.
			fmt.Fprintf(out, "retry them: headroom login %s\n", strings.Join(retry, " "))
		}
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
		st := health(a.ConfigDir)
		r := &loginRow{
			acct:   a,
			email:  loginEmail(a, st),
			health: resolveHealth(st, raw, blob, ok, now.UnixMilli()),
			blob:   blob,
			blobOK: ok,
			sealed: deps.sealed && deps.inKeychain(a),
		}
		switch {
		case r.sealed:
			r.held = "login kept in the Keychain, which this session cannot read"
		case r.email == "" && ok:
			r.held = "logged in, but its identity unreadable — a new login could not be checked"
		}
		rows[i] = r
	}
	return rows
}

// loginEmail is the address an account's login is for. An extra's dir is
// named by the subscription it holds, so the name is the intent even when
// the dir is logged in as someone else; the primary has no such name and is
// whoever its identity document says, or failing that the vendor's own
// status. "" means nobody: a primary never logged in, or one whose identity
// nothing could read.
func loginEmail(a accounts.Account, st auth.Status) string {
	if !a.IsPrimary() {
		return a.Name
	}
	if a.Email == "" && st.Answered() && st.LoggedIn {
		return st.Email
	}
	return a.Email
}

// chooseLogins marks the accounts to renew. Names and --all choose outright;
// otherwise only positive evidence does — no login, an end already passed,
// or an end inside the window. An expiry that is absent or unreadable is
// never read as near, the same rule the board's health follows.
func chooseLogins(rows []*loginRow, named map[string]bool, all bool, within time.Duration, now time.Time) {
	for _, r := range rows {
		state := loginState(r, now)
		switch {
		case r.held != "":
			r.why = r.held
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

// readBack decides whether a login took, against the credential as it stood
// just before: the vendor command succeeded, the stored login is a new one,
// and the dir is now logged in as the account it is for. A new login is a new
// refresh expiry — an access token changes whenever any session refreshes
// it, so a token change counts only when the credential records no expiry.
// It returns the verified credential, and "" when the login took.
func readBack(r *loginRow, before creds.Blob, beforeOK bool, runErr error, deps loginDeps) (creds.Blob, string) {
	if runErr != nil {
		return creds.Blob{}, "claude auth login: " + runErr.Error()
	}
	blob, ok := creds.Parse(deps.readRaw(r.acct))
	var renewed bool
	switch {
	case !ok:
	case !beforeOK:
		renewed = true
	case blob.RefreshState == tag.OK:
		renewed = before.RefreshState != tag.OK || blob.RefreshExpiresMS != before.RefreshExpiresMS
	default:
		renewed = blob.Token != before.Token
	}
	if !renewed {
		return blob, "the stored login did not change"
	}
	got := deps.metaEmail(r.acct)
	if r.email != "" && !strings.EqualFold(got, r.email) {
		return blob, fmt.Sprintf("logged in as %s, not %s — run `headroom login %s` again and approve as %s", orNobody(got), r.email, r.acct.Name, r.email)
	}
	return blob, ""
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
	env := p.Env
	switch {
	case page.remote:
		opener, err := exec.LookPath("true")
		if err != nil {
			return fmt.Errorf("no browser command: %w", err)
		}
		env = append(withoutVars(env, "BROWSER", browser.ProfileEnv), "BROWSER="+opener)
	case runtime.GOOS == "darwin":
		// Profiles are opened with open(1); elsewhere the vendor's own
		// browser handling stays.
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("no browser command: %w", err)
		}
		env = append(withoutVars(env, "BROWSER", browser.ProfileEnv), "BROWSER="+self, browser.ProfileEnv+"="+page.profileDir)
	}
	args := []string{"auth", "login"}
	if email != "" {
		args = append(args, "--email", email)
	}
	cmd := exec.Command(p.Path, args...)
	cmd.Args[0] = p.Binary
	cmd.Env = env
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
