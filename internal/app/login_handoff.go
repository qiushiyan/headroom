package app

// `headroom login start | finish | status | cancel`: one account's sign-in
// handed across commands that each return, for an approver elsewhere — an
// agent with shell access cannot keep a terminal open between its calls.
// `start` hands the sign-in to a detached holder (`login hold`, this file
// too), which runs the vendor's `claude auth login` with headroom as a
// $BROWSER that opens nothing, and prints the URL the vendor printed; the
// approver opens it wherever it is; `finish` hands the holder the code the
// page showed, and prints whether the login took, read back as the batch
// reads it. The holder sends one code, ever, and only one this sign-in's
// state carries. The contract — the document, its states and reasons, the
// exit statuses — is docs/specs/login-handoff.md; the files a sign-in keeps
// are internal/signin's.

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
	"sync"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/browser"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/signin"
	"github.com/qiushiyan/headroom/internal/tag"
)

// handoffBounds bound a handed-off sign-in. None is a measured vendor
// expiry; each is how long headroom waits before it calls the step over.
type handoffBounds struct {
	url      time.Duration // for the vendor to print its sign-in URL
	opener   time.Duration // then for its $BROWSER call to record the state (~200ms measured)
	awaiting time.Duration // for the code, from the start
	exchange time.Duration // for the vendor, once it has the code
	stop     time.Duration // between SIGTERM and a kill
	reply    time.Duration // for a finish's answer: the exchange and the read-back
	status   time.Duration // for a holder's answer to status
	start    time.Duration // for another start or cancel of the account to finish
}

var defaultHandoff = handoffBounds{
	url: 30 * time.Second, opener: 3 * time.Second, awaiting: 10 * time.Minute,
	exchange: 60 * time.Second, stop: 3 * time.Second, reply: 90 * time.Second, status: 5 * time.Second, start: 45 * time.Second,
}

// The reasons a handed-off sign-in names, beyond the held ones and the
// read-back's (login.go).
const (
	reasonBusy          = "busy"
	reasonExchanging    = "exchanging"
	reasonNoURL         = "no_url"
	reasonScreenChanged = "screen_changed"
	reasonHolderFailed  = "holder_failed"
	reasonMalformed     = "code_malformed"
	reasonOtherSignin   = "code_other_signin"
	reasonKeychainShut  = "keychain_closed"
	reasonExpired       = "expired"
	reasonCancelled     = "cancelled"
	reasonHolderGone    = "holder_gone"
	reasonUnknown       = "outcome_unknown"
	reasonNoReply       = "no_reply"
	reasonProtocol      = "protocol"
	reasonNoAttempt     = "no_attempt"
	reasonVendorEnded   = "vendor_ended"
	reasonRecordFailed  = "record_failed"
)

// vendorRun is the vendor's login as a holder runs it.
type vendorRun interface {
	// Lines is its output, a line at a time — the prompt it leaves without a
	// newline included — closed when the output ends.
	Lines() <-chan string
	// Send writes the code to its input.
	Send(code string) error
	// Exited delivers its exit once.
	Exited() <-chan error
	// Stop ends it — SIGTERM, then a kill — and returns once it has gone.
	Stop()
}

// handoffDeps are the edges a handed-off sign-in crosses: the batch's reads
// of an account's login, the vendor's login process, process identity, the
// holder process itself and the clock — injected so the protocol runs in one
// test process with a vendor of the test's.
type handoffDeps struct {
	login  loginDeps
	vendor func(acct accounts.Account, set accounts.Set, email, opener string) (vendorRun, error)
	ps     func(pid int) (int64, error)
	// spawn starts the holder for the named account with the login lock
	// handed over, and returns the record it published once ready.
	spawn  func(name string, lock *os.File, paths signin.Paths) (signin.Record, error)
	now    func() time.Time
	bounds handoffBounds
}

func liveHandoff(scope config.Scope, logw io.Writer) handoffDeps {
	d := handoffDeps{
		login: loginDeps{
			health:       round.ProbeHealth,
			readCred:     liveReadCred,
			metaEmail:    liveMetaEmail,
			keychainItem: liveKeychainItem,
			sealed:       liveSealed,
			signin:       func(accounts.Account) string { return signin.None },
		},
		ps:     sessions.PS,
		now:    time.Now,
		bounds: defaultHandoff,
	}
	d.vendor = func(acct accounts.Account, set accounts.Set, email, opener string) (vendorRun, error) {
		return startVendor(acct, set, email, opener, logw, d.bounds.stop)
	}
	d.spawn = func(name string, lock *os.File, paths signin.Paths) (signin.Record, error) {
		return spawnHolder(name, lock, paths, d.bounds.url+d.bounds.opener+10*time.Second)
	}
	return d
}

// LoginStart starts the named account's sign-in for an approver elsewhere and
// prints its document: the URL to open, while it waits for the code.
func LoginStart(std Streams, scope config.Scope, name string) error {
	return loginStart(std, scope, name, liveHandoff(scope, std.Err))
}

// LoginFinish hands the named account's sign-in the code read from stdin,
// and prints how it ended.
func LoginFinish(std Streams, scope config.Scope, name string) error {
	return loginFinish(std, scope, name, liveHandoff(scope, std.Err))
}

// LoginStatus prints the named account's sign-in as it stands.
func LoginStatus(std Streams, scope config.Scope, name string) error {
	return loginStatus(std, scope, name, liveHandoff(scope, std.Err))
}

// LoginCancel ends the named account's sign-in, if one waits for its code.
func LoginCancel(std Streams, scope config.Scope, name string) error {
	return loginCancel(std, scope, name, liveHandoff(scope, std.Err))
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

// handoffAccount is the named account and its sign-in's files.
func handoffAccount(scope config.Scope, name string) (accounts.Set, accounts.Account, signin.Paths, error) {
	set := accounts.Discover(scope)
	acct, err := set.Select(name)
	if err != nil {
		return set, acct, signin.Paths{}, err
	}
	paths := signin.For(scope.AccountsRoot, acct.Dir())
	return set, acct, paths, paths.Ensure()
}

func loginStart(std Streams, scope config.Scope, name string, deps handoffDeps) error {
	_, acct, paths, err := handoffAccount(scope, name)
	if err != nil {
		return err
	}
	release, err := signin.Serialize(paths.Start(), deps.bounds.start)
	if err != nil {
		return err
	}
	defer release()
	lock, ok, err := signin.Acquire(paths.Lock())
	if err != nil {
		return err
	}
	if !ok {
		// Someone runs this account's login: a holder, whose sign-in is
		// returned as it is while it waits for its code, or the batch.
		rec, answered, err := liveRecord(acct, paths, deps.bounds.status)
		if err != nil {
			return err
		}
		switch {
		case answered == holderAnswered && rec.State == signin.Awaiting:
			reused := true
			return printSignin(std, rec, 0, &reused)
		case answered == holderAnswered && rec.State == signin.Exchanging:
			rec.Reason, rec.Problem = reasonExchanging, "a code for "+acct.Name+" is being exchanged — headroom login status "+acct.Name+" says how it ends"
		case answered == holderAnswered:
			rec.Reason, rec.Problem = reasonBusy, "its sign-in is ending — start again in a moment"
		case answered == holderSilent:
			rec.Reason, rec.Problem = reasonBusy, fmt.Sprintf("its holder did not answer within %s — start again in a moment", deps.bounds.status)
		case answered == holderOtherBuild:
			rec.Reason, rec.Problem = reasonProtocol, "a sign-in held by another headroom build waits for its code — finish it with that build, or cancel it"
		default:
			rec.Reason, rec.Problem = reasonBusy, "another headroom login is renewing "+acct.Name+" right now"
		}
		return printSignin(std, rec, 3, new(false))
	}
	defer lock.Close()
	settleStale(paths, deps.ps, deps.bounds, deps.now())
	rec, err := deps.spawn(acct.Name, lock, paths)
	if err != nil {
		// A holder that never got ready may have started a vendor; under the
		// lock still, it is ended now.
		settleStale(paths, deps.ps, deps.bounds, deps.now())
		rec = endedRecord(acct, signin.Failed, reasonHolderFailed, err.Error(), deps.now())
		signin.Write(paths.Record(), rec)
	}
	code := 1
	switch {
	case rec.State == signin.Awaiting && rec.Reason == "":
		code = 0
	case heldReasons[rec.Reason]:
		code = 3
	}
	return printSignin(std, rec, code, new(false))
}

// heldReasons are why an account cannot start a sign-in from this session:
// refused before any approval, exit 3.
var heldReasons = map[string]bool{heldKeychain: true, heldKeychainUnknown: true, heldIdentity: true, reasonBusy: true}

func loginFinish(std Streams, scope config.Scope, name string, deps handoffDeps) error {
	_, acct, paths, err := handoffAccount(scope, name)
	if err != nil {
		return err
	}
	line, _ := bufio.NewReader(std.In).ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		return usagef("finish reads the code#state the sign-in page showed from stdin, and stdin was empty")
	}
	rep, err := signin.Call(paths.Socket(), signin.Request{Op: signin.OpFinish, Code: code}, deps.bounds.reply)
	rec := rep.Record
	switch {
	case errors.Is(err, signin.ErrNoHolder):
		if rec, err = settled(acct, paths); err != nil {
			return err
		}
		if rec.State == signin.None {
			rec.Reason, rec.Problem = reasonNoAttempt, "no sign-in was started for "+acct.Name+" — headroom login start "+acct.Name
		}
	case errors.Is(err, signin.ErrProtocol):
		if rec, err = onDisk(acct, paths); err != nil {
			return err
		}
		rec.Reason, rec.Problem = reasonProtocol, "the sign-in waiting is held by another headroom build; nothing was sent — finish it with that build, or cancel it"
	case err != nil:
		lost := err
		if rec, err = onDisk(acct, paths); err != nil {
			return err
		}
		rec.Reason, rec.Problem = reasonNoReply, "the holder's answer was lost ("+lost.Error()+"); the code may have been sent — headroom login status "+acct.Name+" says how it ended, and finishing again sends nothing"
		return printSignin(std, rec, 1, nil)
	}
	exit := 1
	switch {
	case rec.State == signin.Renewed:
		exit = 0
	case rec.State == signin.Awaiting, rec.Reason == reasonExchanging:
		// Refused, nothing sent by this call: fix what the reason names and
		// finish again — or, while a code is being exchanged, ask status.
		exit = 3
	}
	return printSignin(std, rec, exit, nil)
}

func loginStatus(std Streams, scope config.Scope, name string, deps handoffDeps) error {
	_, acct, paths, err := handoffAccount(scope, name)
	if err != nil {
		return err
	}
	rec, answered, err := liveRecord(acct, paths, deps.bounds.status)
	if err != nil {
		return err
	}
	switch answered {
	case holderOtherBuild:
		rec.Reason, rec.Problem = reasonProtocol, "the sign-in waiting is held by another headroom build"
	case holderSilent:
		rec.Reason, rec.Problem = reasonNoReply, fmt.Sprintf("its holder did not answer within %s; this is the sign-in as it last recorded it — ask again", deps.bounds.status)
	}
	return printSignin(std, rec, 0, nil)
}

func loginCancel(std Streams, scope config.Scope, name string, deps handoffDeps) error {
	_, acct, paths, err := handoffAccount(scope, name)
	if err != nil {
		return err
	}
	release, err := signin.Serialize(paths.Start(), deps.bounds.start)
	if err != nil {
		return err
	}
	defer release()
	rep, err := signin.Call(paths.Socket(), signin.Request{Op: signin.OpCancel}, deps.bounds.reply)
	switch {
	case err == nil && rep.Record.Reason == reasonExchanging:
		return printSignin(std, rep.Record, 3, nil)
	case err == nil:
		return printSignin(std, rep.Record, 0, nil)
	case !errors.Is(err, signin.ErrNoHolder):
		rec, rerr := onDisk(acct, paths)
		if rerr != nil {
			return rerr
		}
		rec.Reason, rec.Problem = reasonNoReply, "its holder did not answer ("+err.Error()+") — cancel again"
		if errors.Is(err, signin.ErrProtocol) {
			rec.Reason, rec.Problem = reasonProtocol, "the sign-in waiting is held by another headroom build — cancel it with that build"
		}
		return printSignin(std, rec, 3, nil)
	}
	lock, ok, err := signin.Acquire(paths.Lock())
	if err != nil {
		return err
	}
	if !ok {
		rec, err := onDisk(acct, paths)
		if err != nil {
			return err
		}
		rec.Reason, rec.Problem = reasonBusy, "another headroom login is renewing "+acct.Name+" right now; there is no sign-in to cancel"
		return printSignin(std, rec, 3, nil)
	}
	defer lock.Close()
	settleStale(paths, deps.ps, deps.bounds, deps.now())
	rec, err := onDisk(acct, paths)
	if err != nil {
		return err
	}
	return printSignin(std, rec, 0, nil)
}

// Who answered for a sign-in.
const (
	holderAnswered   = iota // its holder, live
	holderOtherBuild        // a holder that speaks another protocol
	holderSilent            // a holder took the call and did not answer in time: slow, not gone
	noHolder                // nothing listens: the record, settled, answers
)

// liveRecord is the account's sign-in as its holder answers for it, or as
// its record says: as written when a holder took the call but did not answer
// in time — it is busy, not gone — and settled when nothing listens, which
// is how a sign-in whose holder is gone ended. An unreadable record is an
// error, never a sign-in that was not started.
func liveRecord(acct accounts.Account, paths signin.Paths, wait time.Duration) (signin.Record, int, error) {
	rep, err := signin.Call(paths.Socket(), signin.Request{Op: signin.OpStatus}, wait)
	switch {
	case err == nil:
		return rep.Record, holderAnswered, nil
	case errors.Is(err, signin.ErrProtocol):
		rec, err := onDisk(acct, paths)
		return rec, holderOtherBuild, err
	case errors.Is(err, signin.ErrNoHolder):
		rec, err := settled(acct, paths)
		return rec, noHolder, err
	}
	rec, err := onDisk(acct, paths)
	return rec, holderSilent, err
}

// onDisk is the account's record as written, or a sign-in never started.
func onDisk(acct accounts.Account, paths signin.Paths) (signin.Record, error) {
	rec, found, err := signin.Read(paths.Record())
	switch {
	case err != nil:
		return signin.Record{}, fmt.Errorf("the sign-in record of %s could not be read: %w", acct.Name, err)
	case !found:
		return signin.Record{Account: acct.Name, Email: loginEmail(acct, auth.Status{}), State: signin.None}, nil
	}
	return rec, nil
}

// settled is the account's record with no holder behind it. Read only; the
// next start or cancel writes it so.
func settled(acct accounts.Account, paths signin.Paths) (signin.Record, error) {
	rec, err := onDisk(acct, paths)
	return settle(rec), err
}

// settle is a record whose holder is gone: a sign-in it calls live ended
// when its holder did — before a code was sent, or with the outcome of one
// unknown.
func settle(rec signin.Record) signin.Record {
	switch rec.State {
	case signin.Awaiting:
		rec.State, rec.Reason, rec.Problem, rec.URL = signin.Failed, reasonHolderGone, "its holder ended before any code was sent — headroom login start "+rec.Account+" begins afresh", ""
	case signin.Exchanging:
		rec.State, rec.Reason, rec.Problem = signin.Unknown, reasonUnknown, "its holder ended while the code was being exchanged — headroom login --dry-run shows whether the login took"
	}
	return rec
}

// settleStale ends what a dead owner left behind; the caller holds the
// login lock, so no owner is alive. A vendor login is registered from its
// launch by whoever ran it — a holder, before any URL, or the batch — and
// outlives that owner (measured: input at end of file does not end `claude
// auth login`), so a registered vendor still running with its recorded
// start is ended: after the exchange bound when its sign-in says a code may
// be in flight. A record a holder left live is then settled.
func settleStale(paths signin.Paths, ps func(int) (int64, error), bounds handoffBounds, now time.Time) {
	rec, found, err := signin.Read(paths.Record())
	exchanging := found && err == nil && rec.State == signin.Exchanging
	if vendor, ok := signin.ReadVendor(paths.Vendor()); ok {
		alive := func() bool {
			start, err := ps(vendor.PID)
			return err == nil && start == vendor.Start
		}
		if exchanging {
			for deadline := time.Now().Add(bounds.exchange); alive() && time.Now().Before(deadline); {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if alive() {
			syscall.Kill(vendor.PID, syscall.SIGTERM)
			for deadline := time.Now().Add(bounds.stop); alive() && time.Now().Before(deadline); {
				time.Sleep(100 * time.Millisecond)
			}
			if alive() {
				syscall.Kill(vendor.PID, syscall.SIGKILL)
			}
		}
		os.Remove(paths.Vendor())
	}
	if found && err == nil && signin.Live(rec.State) {
		gone := settle(rec)
		gone.FinishedMS = now.UnixMilli()
		signin.Write(paths.Record(), gone)
	}
}

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
		SigninState: state, Holder: self,
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
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sig)
	deadline := time.NewTimer(deps.bounds.awaiting)
	defer deadline.Stop()

	lines, exited := v.Lines(), v.Exited()
	before, beforeOK := row.blob, row.blobOK
	var exchange <-chan time.Time
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

		case <-exchange:
			exchange = nil
			v.Stop()
			waitExit()
			drain()
			r := verdict(nil)
			if r.Reason == notRenewed {
				r.Problem = fmt.Sprintf("claude auth login had the code for %s without finishing, and the stored login did not change", deps.bounds.exchange)
			}
			return end(r)

		case <-deadline.C:
			if rec.State != signin.Awaiting {
				continue
			}
			v.Stop()
			waitExit()
			return end(ended(signin.Failed, reasonExpired, fmt.Sprintf("no code came within %s of the start — headroom login start %s begins afresh", deps.bounds.awaiting, acct.Name)))

		case <-sig:
			if rec.State == signin.Exchanging {
				continue // the exchange is bounded; its answer is owed
			}
			v.Stop()
			waitExit()
			return end(ended(signin.Failed, reasonCancelled, "the holder was stopped before any code was sent"))

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
				v.Stop()
				waitExit()
				waiting = c
				return end(ended(signin.Failed, reasonCancelled, "cancelled by headroom login cancel"))

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
				deadline.Stop()
				exchange = time.After(deps.bounds.exchange)

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
	raw, source := deps.readCred(acct)
	item, said := deps.keychainItem(acct)
	switch {
	case keychainWithheld(item, source):
		return "", reasonKeychainShut, "this session can no longer read " + acct.Name + "'s login in the Keychain, so a new one would be stored where sessions do not read it; nothing was sent — run `security unlock-keychain` here and finish again, or cancel"
	case item == creds.ItemUnknown:
		return "", heldKeychainUnknown, "security could not say whether " + acct.Name + "'s login is in the Keychain (" + said + "); nothing was sent — finish again"
	}
	return raw, "", ""
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

// endedRecord is a sign-in that ended as it began.
func endedRecord(acct accounts.Account, state, reason, problem string, now time.Time) signin.Record {
	return signin.Record{Account: acct.Name, Email: loginEmail(acct, auth.Status{}), State: state, Reason: reason, Problem: problem, FinishedMS: now.UnixMilli()}
}

// signinDocument is what start, finish, status and cancel print: the record,
// less what only a holder needs, every field present and null when unknown.
type signinDocument struct {
	Schema      int     `json:"schema"`
	Account     string  `json:"account"`
	Email       *string `json:"email"`
	State       string  `json:"state"`
	Reason      *string `json:"reason"`
	Problem     *string `json:"problem"`
	URL         string  `json:"url,omitempty"`
	StartedAt   *string `json:"started_at"`
	ExpiresAt   *string `json:"expires_at"`
	FinishedAt  *string `json:"finished_at"`
	LoginEndsAt *string `json:"login_ends_at"`
	Reused      *bool   `json:"reused,omitempty"`
}

func printSignin(std Streams, rec signin.Record, code int, reused *bool) error {
	orNull := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	when := func(ms int64) *string {
		if ms == 0 {
			return nil
		}
		return orNull(time.UnixMilli(ms).UTC().Format(time.RFC3339))
	}
	doc := signinDocument{
		Schema: 1, Account: rec.Account, Email: orNull(rec.Email), State: rec.State,
		Reason: orNull(rec.Reason), Problem: orNull(rec.Problem),
		StartedAt: when(rec.StartedMS), ExpiresAt: when(rec.ExpiresMS), FinishedAt: when(rec.FinishedMS),
		LoginEndsAt: when(rec.LoginEndsMS), Reused: reused,
	}
	if rec.State == signin.Awaiting {
		doc.URL = rec.URL
	}
	if err := writeDocument(std.Out, doc); err != nil {
		return err
	}
	if code != 0 {
		return Status(code)
	}
	return nil
}

// writeDocument prints a login document indented, with `&` and its kin left
// as they are: the URL in it is copied out by callers that do not decode
// JSON escapes.
func writeDocument(w io.Writer, doc any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// liveVendor is `claude auth login` run by a holder: input on a pipe the
// holder writes the code to, output — both streams — read off a pipe a line
// at a time and copied to the holder's log.
type liveVendor struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	lines  chan string
	exited chan error
	gone   chan struct{} // closed once the process is reaped
	grace  time.Duration
	stop   sync.Once
}

func (v *liveVendor) Lines() <-chan string { return v.lines }
func (v *liveVendor) Exited() <-chan error { return v.exited }

func (v *liveVendor) Send(code string) error {
	_, err := io.WriteString(v.in, code+"\n")
	return err
}

// Stop ends the vendor and returns once it has gone: SIGTERM, which it exits
// on, then a kill after grace. A holder about to return does not leave the
// kill to a goroutine that would die with it.
func (v *liveVendor) Stop() {
	v.stop.Do(func() {
		v.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-v.gone:
		case <-time.After(v.grace):
			v.cmd.Process.Kill()
			<-v.gone
		}
	})
}

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
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	w.Close()
	forget, err := registerVendor(set.Scope, acct, cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		r.Close()
		return nil, err
	}
	v := &liveVendor{cmd: cmd, in: in, lines: make(chan string, 64), exited: make(chan error, 1), gone: make(chan struct{}), grace: grace}
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
		err := cmd.Wait()
		forget()
		close(v.gone)
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
			return signin.Record{}, fmt.Errorf("the sign-in's holder ended before it was ready — its log is %s", paths.Log())
		}
		return rec, nil
	case <-time.After(wait):
		cmd.Process.Kill()
		cmd.Process.Release()
		return signin.Record{}, fmt.Errorf("the sign-in's holder was not ready within %s — its log is %s", wait, paths.Log())
	}
}
