package app

// `headroom login hold`: the holder, the detached process a handed-off
// sign-in lives in, from the vendor it starts to the record it ends with. The
// commands that start it and ask it are login_handoff.go's.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/signin"
	"github.com/qiushiyan/headroom/internal/tag"
)

// notifySignals is the holder's signals: the ones a session ending or a
// person stopping it sends.
func notifySignals(c chan<- os.Signal) (stop func()) {
	signal.Notify(c, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	return func() { signal.Stop(c) }
}

// LoginHold is the holder `login start` runs detached: the login lock on
// descriptor 3, its document out on stdout once ready, its log on stderr.
func LoginHold(std Streams, scope config.Scope, name string) error {
	lock := os.NewFile(3, "login lock")
	// The lock must be the one start took and handed over; run any other way,
	// the holder would hold a sign-in nothing else knows of.
	if lock == nil || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("login hold is run by headroom login start, with the account's login lock")
	}
	// Held, and kept reachable, for the holder's whole life: an *os.File
	// nothing refers to is closed by the collector, and the lock with it.
	defer lock.Close()
	// The holder's alone: a vendor that inherited it would keep the lock
	// past a holder that died, and with it the next start from ending that
	// vendor.
	syscall.CloseOnExec(int(lock.Fd()))
	ready, _ := std.Out.(io.WriteCloser)
	if ready == nil {
		ready = nopCloser{std.Out}
	}
	return holdSignin(ready, std.Err, scope, name, liveHandoff(scope, std.Err))
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// holdSignin is a holder's whole life: it starts the vendor's login for the
// account, publishes the sign-in once the vendor has printed its URL — on
// ready, which it then closes, so the start that waits on it returns — and
// serves the socket until the sign-in ends: one code exchanged and read
// back, a cancel, the deadline, or the vendor ending on its own.
func holdSignin(ready io.WriteCloser, logw io.Writer, scope config.Scope, name string, deps handoffDeps) error {
	publish := func(rec signin.Record) {
		data, _ := json.Marshal(rec)
		ready.Write(append(data, '\n'))
		ready.Close()
	}
	set := accounts.Discover(scope)
	acct, err := set.Select(name)
	if err != nil {
		publish(endedRecord(accounts.Account{Name: name}, signin.Failed, reasonHolderFailed, err.Error(), deps.now()))
		return err
	}
	paths := signin.For(scope.AccountsRoot, acct.Dir())
	row := assessLogins([]accounts.Account{acct}, deps.login, deps.now())[0]
	if row.held != "" {
		// Refused before any approval: the record stays as it stood.
		rec, err := onDisk(acct, paths)
		if err != nil {
			rec = endedRecord(acct, signin.Failed, reasonHolderFailed, err.Error(), deps.now())
		}
		rec.Reason, rec.Problem = row.heldReason, heldProblem(row)
		publish(rec)
		return nil
	}
	fail := func(reason, problem string) error {
		rec := endedRecord(acct, signin.Failed, reason, problem, deps.now())
		rec.Log = paths.Log()
		signin.Write(paths.Record(), rec)
		publish(rec)
		return nil
	}
	ln, err := signin.Listen(paths.Socket())
	if err != nil {
		return fail(reasonHolderFailed, err.Error())
	}
	defer ln.Close()
	os.Remove(paths.Opener())
	v, err := deps.vendor(acct, set, row.email, paths.Opener())
	if err != nil {
		return fail(reasonHolderFailed, "claude auth login: "+err.Error())
	}
	url, state, last, ok := awaitSigninURL(v, deps.bounds.url)
	if !ok {
		v.Stop()
		problem := fmt.Sprintf("claude auth login printed no sign-in URL headroom could read within %s", deps.bounds.url)
		if last != "" {
			problem += " (its last line: " + last + ")"
		}
		return fail(reasonNoURL, problem+" — has Claude Code changed its login screen? Nothing was started")
	}
	if opened := awaitOpener(paths.Opener(), deps.bounds.opener); opened != "" && opened != state {
		v.Stop()
		return fail(reasonScreenChanged, "the URL claude auth login printed is not the sign-in it opened (state "+statePrefix(state)+" printed, "+statePrefix(opened)+" opened) — has Claude Code changed its login screen? Nothing was started")
	}
	now := deps.now()
	self := signin.Proc{PID: os.Getpid()}
	self.Start, _ = deps.ps(self.PID)
	rec := signin.Record{
		Account: acct.Name, Email: row.email, State: signin.Awaiting, URL: url,
		StartedMS: now.UnixMilli(), ExpiresMS: now.Add(deps.bounds.awaiting).UnixMilli(),
		SigninState: state, Holder: self, Log: paths.Log(),
	}
	if err := signin.Write(paths.Record(), rec); err != nil {
		v.Stop()
		return fail(reasonHolderFailed, err.Error())
	}
	publish(rec)
	return serveSignin(ln, v, rec, acct, row, paths, deps, logw)
}

// awaitSigninURL reads the vendor's screen until it prints the sign-in URL,
// within wait; last is the last line it printed, for a screen that never
// shows one.
func awaitSigninURL(v vendorRun, wait time.Duration) (url, state, last string, ok bool) {
	timeout := time.After(wait)
	lines := v.Lines()
	for {
		select {
		case line, open := <-lines:
			if !open {
				return "", "", last, false
			}
			if u, st, found := browser.SigninURL(line); found {
				return u, st, last, true
			}
			if t := strings.TrimSpace(line); t != "" {
				last = t
			}
		case <-timeout:
			return "", "", last, false
		}
	}
}

// awaitOpener is the state headroom recorded as the vendor's $BROWSER, ""
// when none came within wait.
func awaitOpener(path string, wait time.Duration) string {
	for deadline := time.Now().Add(wait); ; time.Sleep(50 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		if time.Now().After(deadline) {
			return ""
		}
	}
}

// serveSignin answers the socket until the sign-in ends, and records each
// step before it is acted on: `exchanging` is written before the code goes
// to the vendor, so a holder that dies mid-exchange leaves a record that
// says the outcome is unknown, never one that invites a second code.
func serveSignin(ln *net.UnixListener, v vendorRun, rec signin.Record, acct accounts.Account, row *loginRow, paths signin.Paths, deps handoffDeps, logw io.Writer) error {
	done := make(chan struct{})
	defer close(done)
	conns := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case conns <- c:
			case <-done:
				c.Close()
				return
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	defer deps.signals(sig)()
	// One bound at a time: the wait for a code, then, once one is sent, the
	// exchange.
	bound := time.NewTimer(deps.bounds.awaiting)
	defer bound.Stop()

	lines, exited := v.Lines(), v.Exited()
	before, beforeOK := row.blob, row.blobOK
	var waiting net.Conn // the finish whose code is being exchanged
	last := ""
	note := func(line string) {
		// The prompt carries no newline, so what follows it shares its line.
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Paste code here if prompted >"))
		if line != "" {
			last = line
		}
	}
	drain := func() {
		for lines != nil {
			select {
			case line, open := <-lines:
				if !open {
					lines = nil
					continue
				}
				note(line)
			case <-time.After(time.Second):
				return
			}
		}
	}
	waitExit := func() error {
		if exited == nil {
			return nil
		}
		select {
		case err := <-exited:
			exited = nil
			return err
		case <-time.After(deps.bounds.stop + 2*time.Second):
			return nil
		}
	}
	// record makes r the sign-in only once it is on disk: a step a dead
	// holder's record would not show is a step not taken.
	record := func(r signin.Record) error {
		if err := signin.Write(paths.Record(), r); err != nil {
			fmt.Fprintf(logw, "headroom: the sign-in record could not be written: %v\n", err)
			return err
		}
		rec = r
		return nil
	}
	end := func(r signin.Record) error {
		r.URL, r.FinishedMS = "", deps.now().UnixMilli()
		if err := record(r); err != nil {
			// The outcome is still this one; the record keeps the last step it
			// could, and status will read that.
			note := fmt.Sprintf("its record could not be saved (%v), so status will not show this outcome", err)
			if r.Problem == "" {
				r.Problem = note
			} else {
				r.Problem += "; " + note
			}
		}
		if waiting != nil {
			signin.Answer(waiting, signin.Reply{Record: r})
		}
		return nil
	}
	ended := func(state, reason, problem string) signin.Record {
		r := rec
		r.State, r.Reason, r.Problem = state, reason, problem
		return r
	}
	// stop ends a sign-in no code reached: the vendor, then the record.
	stop := func(reason, problem string) error {
		v.Stop()
		waitExit()
		return end(ended(signin.Failed, reason, problem))
	}
	// verdict reads the login back as the batch does. runErr is the vendor's
	// own failure, nil when headroom ended a vendor that went quiet.
	verdict := func(runErr error) signin.Record {
		if runErr != nil && last != "" {
			runErr = fmt.Errorf("%s (%v)", last, runErr)
		}
		blob, msg, reason := judgeLogin(row, before, beforeOK, runErr, deps.login)
		if msg != "" {
			return ended(signin.Failed, reason, msg+" — headroom login start "+acct.Name+" begins a new sign-in")
		}
		r := ended(signin.Renewed, "", "")
		if blob.RefreshState == tag.OK {
			r.LoginEndsMS = blob.RefreshExpiresMS
		}
		return r
	}

	for {
		select {
		case line, open := <-lines:
			if !open {
				lines = nil
				continue
			}
			note(line)

		case err := <-exited:
			exited = nil
			drain()
			r := verdict(err)
			if rec.State == signin.Awaiting && r.State == signin.Failed {
				// It ended by itself before any code reached it.
				r.Reason, r.Problem = reasonVendorEnded, "claude auth login ended before any code was sent"
				if last != "" {
					r.Problem += ": " + last
				}
				r.Problem += " — headroom login start " + acct.Name + " begins a new sign-in"
			}
			return end(r)

		case <-bound.C:
			if rec.State == signin.Awaiting {
				return stop(reasonExpired, fmt.Sprintf("no code came within %s of the start — headroom login start %s begins afresh", deps.bounds.awaiting, acct.Name))
			}
			v.Stop()
			waitExit()
			drain()
			r := verdict(nil)
			if r.Reason == notRenewed {
				r.Problem = fmt.Sprintf("claude auth login had the code for %s without finishing, and the stored login did not change", deps.bounds.exchange)
			}
			return end(r)

		case <-sig:
			if rec.State == signin.Exchanging {
				continue // the exchange is bounded; its answer is owed
			}
			return stop(reasonCancelled, "the holder was stopped before any code was sent")

		case c := <-conns:
			req, err := signin.ReadRequest(c, 5*time.Second)
			if errors.Is(err, signin.ErrProtocol) {
				signin.Answer(c, signin.Reply{Record: rec, Error: signin.ErrProtocol.Error()})
				continue
			}
			if err != nil {
				c.Close()
				continue
			}
			switch req.Op {
			case signin.OpStatus:
				signin.Answer(c, signin.Reply{Record: rec})

			case signin.OpCancel:
				if rec.State == signin.Exchanging {
					r := rec
					r.Reason, r.Problem = reasonExchanging, "a code is being exchanged and cannot be called back — headroom login status "+acct.Name+" says how it ends"
					signin.Answer(c, signin.Reply{Record: r})
					continue
				}
				waiting = c
				return stop(reasonCancelled, "cancelled by headroom login cancel")

			case signin.OpFinish:
				if rec.State != signin.Awaiting {
					r := rec
					r.Reason, r.Problem = reasonExchanging, "a code is already being exchanged; nothing more was sent — headroom login status "+acct.Name+" says how it ends"
					signin.Answer(c, signin.Reply{Record: r})
					continue
				}
				raw, reason, problem := judgeSubmission(req.Code, rec, acct, deps.login)
				if reason != "" {
					r := rec
					r.Reason, r.Problem = reason, problem
					signin.Answer(c, signin.Reply{Record: r})
					continue
				}
				// The baseline is taken now: a live session may have
				// refreshed the account since the start.
				r := rec
				r.State = signin.Exchanging
				if err := record(r); err != nil {
					r := rec
					r.Reason, r.Problem = reasonRecordFailed, "the sign-in's record could not be written ("+err.Error()+"), and a code is sent only once its record shows it; nothing was sent — finish again"
					signin.Answer(c, signin.Reply{Record: r})
					continue
				}
				before, beforeOK = creds.Parse(raw)
				if err := v.Send(strings.TrimSpace(req.Code)); err != nil {
					fmt.Fprintf(logw, "headroom: the code could not be written to claude auth login: %v\n", err)
				}
				waiting = c
				bound.Reset(deps.bounds.exchange)

			default:
				c.Close()
			}
		}
	}
}

// judgeSubmission decides whether a code may go to the vendor: shaped
// `code#state`, carrying the state of the URL handed out, and with the
// account's login still read where sessions read it. A code the vendor
// rejects ends the sign-in (measured, 2.1.311), so a code headroom can tell
// is wrong is refused here, without the escape a person at the terminal
// has: the URL was headroom's own, so another state is another sign-in. The
// line itself is never repeated back. raw is the login read just now, the
// read-back's baseline.
func judgeSubmission(code string, rec signin.Record, acct accounts.Account, deps loginDeps) (raw, reason, problem string) {
	c, ok := browser.ParseCode(strings.TrimSpace(code))
	switch {
	case !ok:
		return "", reasonMalformed, "the line sent is not a sign-in code: the page shows one shaped code#state, and all of it is the code; nothing was sent"
	case c.State != rec.SigninState:
		return "", reasonOtherSignin, fmt.Sprintf("that code is from another sign-in (its state begins %s, this one's %s): send the code from the page of the URL headroom login start printed; nothing was sent",
			statePrefix(c.State), statePrefix(rec.SigninState))
	}
	read := deps.readCred(acct)
	switch access, said := read.Access(deps.keychainItem(acct)); access {
	case creds.Withheld:
		return "", reasonKeychainShut, "this session can no longer read " + acct.Name + "'s login in the Keychain, so a new one would be stored where sessions do not read it; nothing was sent — run `security unlock-keychain` here and finish again, or cancel"
	case creds.Unplaced:
		return "", heldKeychainUnknown, "security could not say whether " + acct.Name + "'s login is in the Keychain (" + said + "); nothing was sent — finish again"
	}
	return read.Raw, "", ""
}

// heldProblem is a held account's reason in words, with what opens it.
func heldProblem(r *loginRow) string {
	switch r.heldReason {
	case heldKeychain:
		return r.held + " — nothing was started; run `security unlock-keychain` in this session (this Mac's password), or renew at the machine"
	case heldKeychainUnknown:
		return r.held + " — nothing was started; start again, or renew at the machine if security keeps failing here"
	case heldIdentity:
		return r.held + " — nothing was started; headroom login " + r.acct.Name + " at the machine renews it"
	}
	return r.held
}

// liveVendor is `claude auth login` run by a holder: input on a pipe the
// holder writes the code to, output — both streams — read off a pipe a line
// at a time and copied to the holder's log.
type liveVendor struct {
	*signin.Vendor
	in     io.WriteCloser
	lines  chan string
	exited chan error
	grace  time.Duration
}

func (v *liveVendor) Lines() <-chan string { return v.lines }
func (v *liveVendor) Exited() <-chan error { return v.exited }

func (v *liveVendor) Send(code string) error {
	_, err := io.WriteString(v.in, code+"\n")
	return err
}

func (v *liveVendor) Stop() { v.Vendor.Stop(v.grace) }

// startVendor runs `claude auth login` for acct, built as the batch builds
// it, with headroom as a $BROWSER that records the sign-in's state at opener
// and opens nothing: the approver is elsewhere. It is registered beside the
// account's login lock from its launch, before any URL, until it is reaped.
func startVendor(acct accounts.Account, set accounts.Set, email, opener string, logw io.Writer, grace time.Duration) (vendorRun, error) {
	cmd, err := loginCommand(acct, set, approvalPage{email: email, remote: true}, opener, logw)
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	proc, err := signin.For(set.Scope.AccountsRoot, acct.Dir()).StartVendor(cmd, sessions.PS)
	w.Close()
	if err != nil {
		r.Close()
		return nil, err
	}
	v := &liveVendor{Vendor: proc, in: in, lines: make(chan string, 64), exited: make(chan error, 1), grace: grace}
	read := make(chan struct{})
	go func() {
		defer close(read)
		defer close(v.lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			fmt.Fprintln(logw, "claude: "+sc.Text())
			select {
			case v.lines <- sc.Text():
			default: // nobody reads any more; the log has it
			}
		}
	}()
	go func() {
		err := proc.Wait()
		<-read // its last words reach Lines before its exit does
		r.Close()
		v.exited <- err
	}()
	return v, nil
}

// spawnHolder starts `headroom login hold <name>` in a session of its own,
// with the login lock handed over as descriptor 3, stdin on /dev/null and
// stderr on the sign-in's log, and waits up to wait for the record it
// publishes on stdout. No stream of the caller reaches it, so a caller that
// reads its own output to the end — sshd — is not held open by it.
func spawnHolder(name string, lock *os.File, paths signin.Paths, wait time.Duration) (signin.Record, error) {
	exe, err := os.Executable()
	if err != nil {
		return signin.Record{}, err
	}
	logf, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return signin.Record{}, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "login", "hold", name)
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return signin.Record{}, err
	}
	if err := cmd.Start(); err != nil {
		return signin.Record{}, err
	}
	got := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadBytes('\n')
		got <- line
	}()
	select {
	case line := <-got:
		cmd.Process.Release()
		var rec signin.Record
		if len(line) == 0 || json.Unmarshal(line, &rec) != nil {
			return signin.Record{}, errors.New("the sign-in's holder ended before it was ready")
		}
		return rec, nil
	case <-time.After(wait):
		cmd.Process.Kill()
		cmd.Process.Release()
		return signin.Record{}, fmt.Errorf("the sign-in's holder was not ready within %s", wait)
	}
}
