// Package jsonl keeps the logs headroom appends to and never routes by: one
// JSON object per line, each carrying its time in "at" (RFC3339), bounded by
// age once the file has grown.
//
// The protocol is the package's reason to exist. Appending and the rewrite
// that bounds the file share the log's own lock: appenders hold it shared and
// the rewrite holds it alone, because a rewrite renames a new file over the
// old one and a line appended to the old one meanwhile would be lost. No
// other lock is ever involved — a log is never something a decision waits on.
package jsonl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrBusy is Append's answer when the log is being rewritten and stayed so past
// the wait: the line was not written, and the caller says so.
var ErrBusy = errors.New("log is busy")

const (
	appendWait = 250 * time.Millisecond
	lockPoll   = 10 * time.Millisecond

	// headBytes is how much of the file's start a size check reads to decide
	// whether a rewrite would drop anything. Lines are appended in time
	// order, so the oldest ones are there; reading the whole file to learn
	// that nothing is old enough would make every append above the size bound
	// pay for the whole file.
	headBytes = 64 << 10
)

// Log is one bounded file.
//
// Nothing is rewritten while the file is below MaxBytes, and above it only
// lines older than Keep are dropped. Slack is how much older than Keep the
// oldest line may grow before a rewrite happens: without it, a file in its
// steady state would be rewritten on every append, each time to drop the
// line or two that had just aged out.
type Log struct {
	Path     string
	MaxBytes int64
	Keep     time.Duration
	Slack    time.Duration
}

// Append writes one value as one line, in one write, so concurrent appenders
// interleave whole lines. A failure is the caller's to mention and never to
// act on.
//
// An appender waits only as long as a rewrite takes, then gives up and
// reports ErrBusy.
func (l Log) Append(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := flock(lock, syscall.LOCK_SH, appendWait); err != nil {
		return err
	}
	size, err := appendLine(l.Path, line)
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err == nil && size > l.MaxBytes {
		l.prune(lock, time.Now())
	}
	return err
}

// appendLine writes the line and reports the file's size afterwards. A file
// that does not end in a newline holds a line some writer never finished; the
// line then starts on a line of its own, so the damage costs the damaged line
// and not the one after it.
func appendLine(path string, line []byte) (int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	out := make([]byte, 0, len(line)+2)
	if fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			out = append(out, '\n')
		}
	}
	out = append(append(out, line...), '\n')
	if _, err := f.Write(out); err != nil {
		return 0, err
	}
	return fi.Size() + int64(len(out)), nil
}

// flock takes the lock in the given mode, polling for at most wait.
func flock(f *os.File, how int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrBusy
		}
		time.Sleep(lockPoll)
	}
}

// prune rewrites the log without the lines older than Keep, holding the log's
// lock alone so no appender writes to the file being replaced. It does not
// wait for the lock: an appender holding it is about to make the same check,
// and a rewrite skipped now happens at a later append. A line that does not
// parse, or carries no time, is dropped with the old ones.
func (l Log) prune(lock *os.File, now time.Time) {
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if !l.due(now) {
		return
	}
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return
	}
	cutoff := now.Add(-l.Keep)
	var kept bytes.Buffer
	dropped := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if at, ok := lineAt(line); !ok || at.Before(cutoff) {
			dropped = true
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}
	if !dropped {
		return
	}
	base := strings.TrimSuffix(filepath.Base(l.Path), filepath.Ext(l.Path))
	tmp, err := os.CreateTemp(filepath.Dir(l.Path), base+"-*"+filepath.Ext(l.Path))
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(kept.Bytes()); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), l.Path)
}

// due reports whether a rewrite would drop a line worth rewriting for: a
// complete line at the head of the file older than Keep and Slack, or one that
// does not parse.
func (l Log) due(now time.Time) bool {
	f, err := os.Open(l.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, headBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	head = head[:n]
	if n == headBytes {
		// The last line read may be cut short; only complete lines count.
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			head = head[:i]
		}
	}
	cutoff := now.Add(-l.Keep - l.Slack)
	for _, line := range bytes.Split(head, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if at, ok := lineAt(line); !ok || at.Before(cutoff) {
			return true
		}
	}
	return false
}

func lineAt(line []byte) (time.Time, bool) {
	var head struct {
		At string `json:"at"`
	}
	if json.Unmarshal(line, &head) != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, head.At)
	return at, err == nil
}

// Read decodes every line, oldest first. A line that does not decode, or that
// valid rejects, is skipped and counted: a torn final line during a concurrent
// append is ordinary. An absent file is an empty log.
func Read[T any](path string, valid func(T) bool) (records []T, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r T
		if json.Unmarshal(line, &r) != nil || !valid(r) {
			skipped++
			continue
		}
		records = append(records, r)
	}
	return records, skipped, nil
}
