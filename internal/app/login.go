package app

// `headroom login`: renew the Claude Code logins that end soon, in one pass.
// The login itself is Claude Code's — `claude auth login`, run under the
// account's environment — so headroom still writes no credential. What
// headroom adds is the choice of accounts, the browser profile the approval
// page opens in, and the read-back that says whether each login took. Spec:
// docs/specs/login.md.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/tag"
)

// defaultLoginWithin is how close a login's end must be for a bare
// `headroom login` to renew it. Claude Code warns at three days; a week
// leaves room to renew a batch together, which is the point.
const defaultLoginWithin = 7 * 24 * time.Hour

// loginDeps are the edges `headroom login` crosses — the health probe, the
// credential and identity reads, the Keychain's reach, where approval pages
// go, and the vendor's login process — injected so selection and read-back
// are testable without a Keychain, a browser or a network.
type loginDeps struct {
	health    func([]accounts.Account) auth.QueryFunc
	readRaw   func(accounts.Account) string
	metaEmail func(accounts.Account) string
	// sealed: the login Keychain refuses this session, as macOS refuses an
	// ssh session. inKeychain reports an account's item by its attributes,
	// which a sealed Keychain still shows.
	sealed     bool
	inKeychain func(accounts.Account) bool
	approver   approver
	now        func() time.Time
	login      func(acct accounts.Account, set accounts.Set, page approvalPage) error

	// tokens are the stored headless tokens, whose ends the plan reports the
	// way it reports logins'.
	tokens func() (map[string]headless.Token, error)
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
	why    string       // why chosen, or why left alone
	page   approvalPage // where its approval goes, for the email it is for
}

// LoginOptions say which logins `headroom login` renews: the named ones, or
// --all, or else each one that ends within Within, has ended or was never
// made. DryRun prints the plan only.
type LoginOptions struct {
	Names  []string
	All    bool
	Within time.Duration
	DryRun bool
}

// DefaultLoginWithin is how close a login's end must be for a bare
// `headroom login` to renew it.
const DefaultLoginWithin = defaultLoginWithin

// Login renews Claude Code logins, one `claude auth login` per account in
// turn, its approval page opened in the Chrome profile matched to the
// account's email, and reads each result back.
func Login(std Streams, scope config.Scope, opts LoginOptions) error {
	return renewLogins(std, scope, opts, loginDeps{
		health: round.ProbeHealth,
		readRaw: func(a accounts.Account) string {
			raw, _ := creds.ReadRaw(context.Background(), a.ConfigDir, a.Dir())
			return raw
		},
		metaEmail: func(a accounts.Account) string {
			email, _ := accounts.MetaEmail(a.MetaPath())
			return email
		},
		sealed:     creds.KeychainSealed(),
		inKeychain: func(a accounts.Account) bool { return creds.HasKeychainItem(a.ConfigDir) },
		approver:   newApprover(scope),
		now:        time.Now,
		login:      newVendorLogin(std),
		tokens:     func() (map[string]headless.Token, error) { return headless.Read(scope.AccountsRoot) },
	})
}

func renewLogins(std Streams, scope config.Scope, opts LoginOptions, deps loginDeps) error {
	out, names, all, within := std.Out, opts.Names, opts.All, opts.Within
	set := accounts.Discover(scope)
	named := map[string]bool{}
	for _, n := range names {
		a, err := set.Select(n)
		if err != nil {
			return err
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
			line = "→" + line[1:] + " — " + r.page.phrase()
		}
		if r.sealed {
			sealed++
		}
		if r.held != "" && (named[r.acct.Name] || all) {
			refused++
		}
		fmt.Fprintln(out, line)
	}
	writeTokenEnds(out, rows, deps, within, now)
	if sealed > 0 {
		fmt.Fprintf(out, "\nthe login Keychain is closed to this ssh session: the %d accounts marked above keep their\n"+
			"login there, so it can be neither read nor renewed from here — a login made here would be\n"+
			"saved to a file that every session able to read the Keychain ignores. Run\n"+
			"`security unlock-keychain` in this session first (it asks for this Mac's password), or\n"+
			"run headroom login at the machine.\n", sealed)
	}
	if len(chosen) == 0 {
		if refused > 0 {
			return Status(1)
		}
		if sealed == 0 {
			fmt.Fprintf(out, "nothing to renew within %s; name an account or pass --all to renew anyway\n", render.Days(within))
		}
		return nil
	}
	if opts.DryRun {
		return nil
	}

	failed := refused
	var retry []string // names whose login ran and did not take
	var earliest int64 // the soonest end among the logins that took
	for i, r := range chosen {
		if r.page.remote {
			fmt.Fprintf(out, "\n[%d/%d] %s — %s\n", i+1, len(chosen), r.acct.Name, r.page.remoteSteps())
		} else {
			fmt.Fprintf(out, "\n[%d/%d] %s — click Authorize in the page that opens\n", i+1, len(chosen), r.acct.Name)
		}
		// The baseline is taken now, not when the batch was planned: a live
		// session may have refreshed this account while earlier approvals
		// were pending.
		before, beforeOK := creds.Parse(deps.readRaw(r.acct))
		err := deps.login(r.acct, set, r.page)
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
		fmt.Fprintf(out, "\nthe logins renewed here end from %s\n", render.When(earliest, deps.now()))
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d of %d logins did not take\n", failed, len(chosen)+refused)
		if len(retry) > 0 {
			// Only the logins that ran and failed: retrying a held account is
			// refused again until what holds it changes.
			fmt.Fprintf(out, "retry them: headroom login %s\n", strings.Join(retry, " "))
		}
		return Status(1)
	}
	return nil
}

// writeTokenEnds names the headless tokens that end inside the window or have
// ended, under the plan that names the logins that do: a token is renewed by
// minting it again, which takes an approval like a login's, and a person
// learns that one is due where they learn that a login is. Quiet when none is.
func writeTokenEnds(out io.Writer, rows []*loginRow, deps loginDeps, within time.Duration, now time.Time) {
	tokens, err := deps.tokens()
	if err != nil {
		fmt.Fprintf(out, "\nheadless tokens: %v\n", err)
		return
	}
	var lines []string
	for _, r := range rows {
		tok, ok := tokens[r.acct.Name]
		if !ok || tok.EndsMS == 0 || time.UnixMilli(tok.EndsMS).Sub(now) >= within {
			continue
		}
		verb := "ends"
		if tok.Ended(now) {
			verb = "ended"
		}
		lines = append(lines, fmt.Sprintf("  %-28s headless token %s %s — headroom headless mint %s", r.acct.Name, verb, render.When(tok.EndsMS, now), r.acct.Name))
	}
	if len(lines) > 0 {
		fmt.Fprintf(out, "\n%s\n", strings.Join(lines, "\n"))
	}
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
			health: round.Health(st, raw, blob, ok, now.UnixMilli()),
			blob:   blob,
			blobOK: ok,
			sealed: deps.sealed && deps.inKeychain(a),
		}
		r.page = deps.approver.page(r.email)
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
		return "login ended " + render.When(r.blob.RefreshExpiresMS, now)
	case accountstate.HealthBadBlob:
		return "credential unreadable (Claude Code changed its format?)"
	case accountstate.HealthUnknown:
		return "login state unknown"
	}
	if !r.blobOK || r.blob.RefreshState != tag.OK {
		return "logged in, end date not recorded"
	}
	return "login ends " + render.When(r.blob.RefreshExpiresMS, now)
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

func endPhrase(b creds.Blob, now time.Time) string {
	if b.RefreshState != tag.OK {
		return ""
	}
	return ", login ends " + render.When(b.RefreshExpiresMS, now)
}

func orNobody(email string) string {
	if email == "" {
		return "nobody"
	}
	return email
}

// vendorLogin runs `claude auth login` for acct in the foreground, under the
// environment launch builds for it, with page's opener as its $BROWSER.
// stdout and stderr are the terminal's: the vendor always prints the URL,
// and asks there for the code when the browser cannot reach its localhost
// callback. The code itself comes in through headroom (relayCodes).
//
// The terminal's input is read line by line from the first login that asks
// until the input ends. One reader serves the whole batch: a read cannot be
// called off, so a reader per login would leave the last one holding the
// next login's paste.
func newVendorLogin(std Streams) func(acct accounts.Account, set accounts.Set, page approvalPage) error {
	pasted := sync.OnceValue(func() <-chan string { return readLines(std.In) })
	return func(acct accounts.Account, set accounts.Set, page approvalPage) error {
		return vendorLogin(std, pasted, acct, set, page)
	}
}

func vendorLogin(std Streams, pasted func() <-chan string, acct accounts.Account, set accounts.Set, page approvalPage) error {
	p, err := launch.Prepare(acct, set, os.Environ())
	if err != nil {
		return err
	}
	for _, n := range p.Notices {
		fmt.Fprintln(std.Err, "headroom: "+n)
	}
	dir, err := os.MkdirTemp("", "headroom-login")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	signin := filepath.Join(dir, "state")
	env, err := page.env(p.Env, signin)
	if err != nil {
		return err
	}
	args := []string{"auth", "login"}
	if page.email != "" {
		args = append(args, "--email", page.email)
	}
	cmd := exec.Command(p.Path, args...)
	cmd.Args[0] = p.Binary
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = std.Out, std.Err
	vendorIn, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	lines := pasted()
	drainLines(lines)
	if err := cmd.Start(); err != nil {
		return err
	}
	done, relayed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(relayed)
		relayCodes(lines, vendorIn, std.Err, func() string {
			// The vendor hands $BROWSER the URL just after printing it, so
			// only a paste typed ahead can beat the record; give it a
			// moment before calling the state unknown. With no opener of
			// headroom's, a pasted code goes unchecked.
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				state, err := os.ReadFile(signin)
				if err == nil || !page.recorded() || time.Now().After(deadline) {
					return string(state)
				}
			}
		}, done)
	}()
	err = cmd.Wait()
	close(done)
	<-relayed
	return err
}

// readLines is r, line by line, until it ends.
func readLines(r io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

// drainLines drops the lines entered before a login began: they were meant
// for an earlier one.
func drainLines(lines <-chan string) {
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// relayCodes hands the vendor the codes pasted at its prompt, until done.
// headroom reads them rather than the vendor, so that a paste echoes and
// edits as a line and a code that is not this sign-in's is refused before it
// is spent — why, in DESIGN.md § Renewing logins ("headroom reads the pasted
// code"). state reports this sign-in's state, "" while unknown. A refused
// line pasted again as it was goes through: the check rests on a reading of
// the vendor and must never be what stands between the owner and a login.
func relayCodes(lines <-chan string, vendor io.WriteCloser, say io.Writer, state func() string, done <-chan struct{}) {
	refused := ""
	for {
		select {
		case <-done:
			return
		case line, ok := <-lines:
			if !ok {
				// The terminal's input ended, and with it the vendor's.
				vendor.Close()
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Fprint(say, "Paste code here > ")
				continue
			}
			problem, note := judgeCode(line, state())
			switch {
			case problem == "":
				fmt.Fprintf(say, "headroom: %s — passing it to claude\n", note)
			case line == refused:
				fmt.Fprintln(say, "headroom: passing it to claude as pasted")
			default:
				refused = line
				fmt.Fprintf(say, "headroom: %s — or paste the same again to send it as it is\nPaste code here > ", problem)
				continue
			}
			refused = ""
			io.WriteString(vendor, line+"\n")
		}
	}
}

// judgeCode reads one trimmed line pasted at the vendor's prompt against the
// sign-in whose state is want ("" unknown): problem is why it should not be
// sent, and otherwise note says what was checked.
func judgeCode(line, want string) (problem, note string) {
	c, ok := browser.ParseCode(line)
	switch {
	case !ok:
		return fmt.Sprintf("%q is not a sign-in code: the page shows one shaped code#state, copy all of it", line), ""
	case want == "":
		return "", "code received"
	case c.State != want:
		return fmt.Sprintf("that code is from another sign-in (its state begins %s, this one's %s): paste the code from the page this login opened or from the URL above",
			statePrefix(c.State), statePrefix(want)), ""
	}
	return "", "code received, from this sign-in"
}

// statePrefix is enough of a state to tell two apart by eye.
func statePrefix(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}
