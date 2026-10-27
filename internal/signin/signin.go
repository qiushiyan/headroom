// Package signin is the machinery of a sign-in that outlives the command
// that starts it — `headroom login start` hands one across commands, and a
// detached holder keeps the vendor's login running between them. Per account
// dir it owns a lock that whoever runs the vendor's login holds, a second
// that serializes starting and cancelling, the record of the sign-in, and the
// socket the holder answers on. It knows nothing of accounts, credentials or
// the vendor: app/login_handoff.go does. The contract is
// docs/specs/login-handoff.md.
package signin

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/atomicfile"
)

// Paths name one account dir's sign-in files, under the accounts root's
// `.login` directory — a dot name, which discovery never takes for an
// account.
type Paths struct{ dir, key string }

// For is the sign-in files of the account whose config dir is accountDir.
// The key is the dir's own, so two homes, or a renamed account, never share
// a sign-in.
func For(accountsRoot, accountDir string) Paths {
	sum := sha256.Sum256([]byte(accountDir))
	return Paths{dir: filepath.Join(accountsRoot, ".login"), key: hex.EncodeToString(sum[:])[:8]}
}

func (p Paths) file(ext string) string { return filepath.Join(p.dir, p.key+ext) }

// Lock is held by whatever runs the vendor's login for the dir.
func (p Paths) Lock() string { return p.file(".lock") }

// Start serializes starting and cancelling the dir's sign-in.
func (p Paths) Start() string { return p.file(".start") }

// Record is the sign-in as it last stood.
func (p Paths) Record() string { return p.file(".json") }

// Socket is where a live holder answers.
func (p Paths) Socket() string { return p.file(".sock") }

// Log is the holder's stderr, kept until the next sign-in of the dir.
func (p Paths) Log() string { return p.file(".log") }

// Opener is where headroom, as the vendor's $BROWSER, records the state of
// the sign-in the vendor opened.
func (p Paths) Opener() string { return p.file(".opener") }

// Vendor names the vendor login process running for the dir, written by
// whoever started it — a holder or the batch — from its launch until it is
// reaped. Whoever next holds the login lock and finds one still running
// ends it: its owner died, and it would otherwise wait for a code forever.
func (p Paths) Vendor() string { return p.file(".vendor") }

// Ensure makes the directory, readable by this user alone: the record names
// a sign-in in progress, and the socket accepts the code that completes it.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.dir, 0o700)
}

// Acquire takes the login lock without waiting. ok is false while another
// process holds it — a holder, for its whole life, or a batch login of the
// dir. The lock is the open file: closing it, or the holder's exit, releases
// it, and a child that inherits the descriptor holds it.
func Acquire(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// Serialize takes the start lock, waiting up to wait: a second `start` waits
// for the first to have its holder ready, then finds it.
func Serialize(path string, wait time.Duration) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(50 * time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("another headroom login start or cancel of this account did not finish within %s", wait)
			}
			return nil, err
		}
	}
}

// The states of a sign-in, as the document names them.
const (
	None       = "none"
	Awaiting   = "awaiting_code"
	Exchanging = "exchanging"
	Renewed    = "renewed"
	Failed     = "failed"
	Unknown    = "unknown"
)

// Live reports whether a sign-in in this state still has a vendor login
// behind it.
func Live(state string) bool { return state == Awaiting || state == Exchanging }

// Proc identifies a process across pid reuse: its pid and the second it
// started at, as ps(1) reports it.
type Proc struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

// Record is a sign-in as its holder last wrote it: what the document prints,
// and what only a holder, or whoever cleans up after one, needs.
type Record struct {
	Account     string `json:"account"`
	Email       string `json:"email"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	Problem     string `json:"problem,omitempty"`
	URL         string `json:"url,omitempty"`
	StartedMS   int64  `json:"started_ms,omitempty"`
	ExpiresMS   int64  `json:"expires_ms,omitempty"`
	FinishedMS  int64  `json:"finished_ms,omitempty"`
	LoginEndsMS int64  `json:"login_ends_ms,omitempty"`

	// SigninState is the OAuth state of the URL handed out, which a code
	// must carry.
	SigninState string `json:"signin_state,omitempty"`
	Holder      Proc   `json:"holder"`
}

// Read loads the record at path; found is false when there is none.
func Read(path string) (rec Record, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{State: None}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return rec, true, nil
}

// Write replaces the record at path whole.
func Write(path string, rec Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, ".record-*", append(data, '\n'))
}

// WriteVendor registers the vendor process just started for the dir.
func WriteVendor(path string, p Proc) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, ".vendor-*", append(data, '\n'))
}

// ReadVendor is the vendor process registered for the dir; ok is false when
// none is.
func ReadVendor(path string) (p Proc, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &p) != nil || p.PID <= 0 {
		return Proc{}, false
	}
	return p, true
}

// Version is the holder protocol this build speaks. A request or reply of
// another is refused before anything is acted on: an old holder may still be
// waiting when a new build is installed.
const Version = 1

// The operations a holder answers.
const (
	OpStatus = "status"
	OpFinish = "finish"
	OpCancel = "cancel"
)

// Request is one call to a holder.
type Request struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Code string `json:"code,omitempty"`
}

// Reply is a holder's answer: the sign-in as it stands after the request,
// with Reason and Problem saying why a finish or cancel did not go as asked.
// Error names a request the holder would not read.
type Reply struct {
	V      int    `json:"v"`
	Record Record `json:"record"`
	Error  string `json:"error,omitempty"`
}

// ErrProtocol is a holder or caller of another protocol version.
var ErrProtocol = errors.New("protocol")

// maxSocketPath is the longest path a unix socket binds at on macOS
// (sun_path is 104 bytes, its NUL included; measured 2026-10-27).
const maxSocketPath = 103

// Listen opens the holder's socket. A stale one — left by a holder that
// died — is removed first; the caller holds the login lock, so no live
// holder answers there.
func Listen(path string) (*net.UnixListener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("the sign-in's socket path %s is %d bytes, longer than the %d a socket can bind at — set a shorter HEADROOM_ACCOUNTS_ROOT", path, len(path), maxSocketPath)
	}
	os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(true)
	return ln, nil
}

// ErrNoHolder is a socket nothing answers on: no sign-in is live there, or
// its holder has died.
var ErrNoHolder = errors.New("no holder answers")

// Call sends req to the holder at path and waits up to wait for its reply.
// ErrNoHolder: nothing listens. Any other error after the request was
// written means its answer was lost, and a finish may have been acted on.
func Call(path string, req Request, wait time.Duration) (Reply, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return Reply{}, ErrNoHolder
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(wait))
	req.V = Version
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Reply{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Reply{}, fmt.Errorf("the holder did not answer: %w", err)
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return Reply{}, fmt.Errorf("the holder's answer: %w", err)
	}
	if rep.V != Version || rep.Error == ErrProtocol.Error() {
		return rep, ErrProtocol
	}
	return rep, nil
}

// ReadRequest reads one request from a caller, refusing another protocol's.
func ReadRequest(conn net.Conn, wait time.Duration) (Request, error) {
	conn.SetReadDeadline(time.Now().Add(wait))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Request{}, err
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Request{}, err
	}
	if req.V != Version {
		return req, ErrProtocol
	}
	return req, nil
}

// Answer writes rep to the caller and ends the call.
func Answer(conn net.Conn, rep Reply) {
	rep.V = Version
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	json.NewEncoder(conn).Encode(rep)
	conn.Close()
}
