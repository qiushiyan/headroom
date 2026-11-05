package app

// `headroom login start | finish | status | cancel`: one account's sign-in
// handed across commands that each return, for an approver elsewhere — an
// agent with shell access cannot keep a terminal open between its calls.
// `start` hands the sign-in to a detached holder (`login hold`,
// login_holder.go), which runs the vendor's `claude auth login` with headroom as a
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
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/signin"
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
	spawn func(name string, lock *os.File, paths signin.Paths) (signin.Record, error)
	// signals delivers the signals that end a holder to c, until the returned
	// stop.
	signals func(c chan<- os.Signal) (stop func())
	now     func() time.Time
	bounds  handoffBounds
}

func liveHandoff(scope config.Scope, logw io.Writer) handoffDeps {
	d := handoffDeps{
		login: loginDeps{
			health:       round.ProbeHealth,
			readCred:     liveReadCred,
			metaEmail:    liveMetaEmail,
			keychainItem: liveKeychainItem,
			sealed:       liveSealed,
			signin:       func(accounts.Account) signin.Record { return signin.Record{State: signin.None} },
		},
		ps:      sessions.PS,
		signals: notifySignals,
		now:     time.Now,
		bounds:  defaultHandoff,
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
		rec, answered, _, err := ask(acct, paths, signin.Request{Op: signin.OpStatus}, deps.bounds.status)
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
		rec.Log = paths.Log()
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
	rec, who, lost, err := ask(acct, paths, signin.Request{Op: signin.OpFinish, Code: code}, deps.bounds.reply)
	if err != nil {
		return err
	}
	switch who {
	case noHolder:
		if rec.State == signin.None {
			rec.Reason, rec.Problem = reasonNoAttempt, "no sign-in was started for "+acct.Name+" — headroom login start "+acct.Name
		}
	case holderOtherBuild:
		rec.Reason, rec.Problem = reasonProtocol, "the sign-in waiting is held by another headroom build; nothing was sent — finish it with that build, or cancel it"
	case holderSilent:
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
	rec, answered, _, err := ask(acct, paths, signin.Request{Op: signin.OpStatus}, deps.bounds.status)
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
	rec, who, lost, err := ask(acct, paths, signin.Request{Op: signin.OpCancel}, deps.bounds.reply)
	switch {
	case err != nil:
		return err
	case who == holderAnswered && rec.Reason == reasonExchanging:
		return printSignin(std, rec, 3, nil)
	case who == holderAnswered:
		return printSignin(std, rec, 0, nil)
	case who == holderOtherBuild:
		rec.Reason, rec.Problem = reasonProtocol, "the sign-in waiting is held by another headroom build — cancel it with that build"
		return printSignin(std, rec, 3, nil)
	case who == holderSilent:
		rec.Reason, rec.Problem = reasonNoReply, "its holder did not answer ("+lost.Error()+") — cancel again"
		return printSignin(std, rec, 3, nil)
	}
	lock, ok, err := signin.Acquire(paths.Lock())
	if err != nil {
		return err
	}
	if !ok {
		rec.Reason, rec.Problem = reasonBusy, "another headroom login is renewing "+acct.Name+" right now; there is no sign-in to cancel"
		return printSignin(std, rec, 3, nil)
	}
	defer lock.Close()
	settleStale(paths, deps.ps, deps.bounds, deps.now())
	if rec, err = onDisk(acct, paths); err != nil {
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

// ask is the account's sign-in as its holder answers req, or as its record
// says: as written when a holder took the call but did not answer in time —
// it is busy, not gone — or speaks another protocol, and settled when
// nothing listens, which is how a sign-in whose holder is gone ended. lost
// is why a holder that took the call gave no answer, for the document to
// quote. An unreadable record is an error, never a sign-in that was not
// started.
func ask(acct accounts.Account, paths signin.Paths, req signin.Request, wait time.Duration) (rec signin.Record, who int, lost, err error) {
	rep, lost := signin.Call(paths.Socket(), req, wait)
	switch {
	case lost == nil:
		return rep.Record, holderAnswered, nil, nil
	case errors.Is(lost, signin.ErrNoHolder):
		rec, err := settled(acct, paths)
		return rec, noHolder, nil, err
	}
	who = holderSilent
	if errors.Is(lost, signin.ErrProtocol) {
		who = holderOtherBuild
	}
	rec, err = onDisk(acct, paths)
	return rec, who, lost, err
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
	inFlight := time.Duration(0)
	if found && err == nil && rec.State == signin.Exchanging {
		inFlight = bounds.exchange
	}
	paths.EndVendor(ps, inFlight, bounds.stop)
	if found && err == nil && signin.Live(rec.State) {
		gone := settle(rec)
		gone.FinishedMS = now.UnixMilli()
		signin.Write(paths.Record(), gone)
	}
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
	problem := rec.Problem
	if (rec.State == signin.Failed || rec.State == signin.Unknown) && rec.Log != "" {
		// What the vendor printed is where an ending is explained; the log
		// holds it until the account's next start.
		problem += " (the holder's log: " + rec.Log + ")"
	}
	doc := signinDocument{
		Schema: 1, Account: rec.Account, Email: orNull(rec.Email), State: rec.State,
		Reason: orNull(rec.Reason), Problem: orNull(strings.TrimSpace(problem)),
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
