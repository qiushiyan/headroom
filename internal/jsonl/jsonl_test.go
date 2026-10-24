package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type line struct {
	At string `json:"at"`
	N  int    `json:"n"`
}

func at(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func read(t *testing.T, path string) []line {
	t.Helper()
	got, _, err := Read(path, func(l line) bool { return l.At != "" })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Below MaxBytes nothing is rewritten, however old: a rewrite on every append
// would be pure contention for a file that gains a little a line. The same
// file, the same append, over its bound drops what is old.
func TestOnlyAFileOverItsBoundIsRewritten(t *testing.T) {
	now := time.Now()
	l := Log{Path: filepath.Join(t.TempDir(), "x.jsonl"), MaxBytes: 1 << 20, Keep: time.Hour, Slack: time.Minute}
	if err := l.Append(line{At: at(now.Add(-2 * time.Hour)), N: -1}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(line{At: at(now), N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, l.Path); len(got) != 2 {
		t.Fatalf("below the size bound an old line was dropped: %+v", got)
	}
	l.MaxBytes = 1
	if err := l.Append(line{At: at(now), N: 2}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, l.Path); len(got) != 2 || got[0].N != 1 || got[1].N != 2 {
		t.Fatalf("over the bound: %+v, want the two recent lines", got)
	}
}

// The log's own lock held alone by someone else — a rewrite in progress: an
// append gives up within its wait and says so, rather than writing to a file
// that is about to be replaced.
func TestAnAppendDuringARewriteGivesUp(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "x.jsonl"), MaxBytes: 1, Keep: time.Hour, Slack: time.Minute}
	if err := l.Append(line{At: at(time.Now()), N: 1}); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := l.Append(line{At: at(time.Now()), N: 2}); err != ErrBusy {
		t.Fatalf("append under a held lock: %v, want ErrBusy", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("an append waited on the log's lock past its bound")
	}
	if got := read(t, l.Path); len(got) != 1 {
		t.Fatalf("with the lock held: %+v, want the one line already there", got)
	}
}

// A line some writer never finished costs that line and nothing else. Read
// skips and counts it — a torn final line during a concurrent append is
// ordinary — and the next append starts on a line of its own, so a write that
// succeeded is a line that reads back. An absent file is an empty log.
func TestATornLineCostsOnlyItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(path, []byte(`{"at":"2026-10-01T10:0`), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := func(l line) bool { return l.At != "" }
	if got, skipped, err := Read(path, valid); err != nil || len(got) != 0 || skipped != 1 {
		t.Fatalf("a torn tail: %d lines, %d skipped, %v", len(got), skipped, err)
	}
	l := Log{Path: path, MaxBytes: 1 << 20, Keep: time.Hour}
	if err := l.Append(line{At: at(time.Now()), N: 1}); err != nil {
		t.Fatal(err)
	}
	if got, skipped, err := Read(path, valid); err != nil || len(got) != 1 || skipped != 1 {
		t.Fatalf("after a torn tail: %d lines, %d skipped, %v", len(got), skipped, err)
	}
	if got, skipped, err := Read(filepath.Join(t.TempDir(), "absent.jsonl"), valid); err != nil || got != nil || skipped != 0 {
		t.Fatalf("an absent log: %v, %d skipped, %v", got, skipped, err)
	}
}

// Appends and the rewrite that bounds the file share one protocol: a line
// whose append succeeded is in the file, however many rewrites ran meanwhile.
// An append may give up under contention — and says so; what it may not do is
// report success for a line the rename then threw away.
func TestAppendsSurviveConcurrentPruning(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "x.jsonl"), MaxBytes: 1, Keep: time.Hour, Slack: time.Minute} // every append is over the bound
	now := time.Now()
	const writers, each = 8, 40
	var wg sync.WaitGroup
	var written atomic.Int32
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				// An old line beside every new one, so each rewrite has
				// something to drop and really does rename.
				_ = l.Append(line{At: at(now.Add(-2 * time.Hour)), N: -1})
				switch err := l.Append(line{At: at(now), N: w*1000 + i}); err {
				case nil:
					written.Add(1)
				case ErrBusy:
				default:
					t.Errorf("append: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	got, skipped, err := Read(l.Path, func(l line) bool { return l.At != "" })
	if err != nil || skipped != 0 {
		t.Fatalf("read: %d skipped, %v", skipped, err)
	}
	kept := 0
	for _, r := range got {
		if r.N >= 0 {
			kept++
		}
	}
	if int(written.Load()) == 0 || kept != int(written.Load()) {
		t.Fatalf("%d appends reported success and %d of their lines are in the file", written.Load(), kept)
	}
}

// A line just past Keep is not worth a rewrite: in a file's steady state that
// would be every append. Only once the oldest line is past Keep and Slack does
// the rewrite run — and then it drops everything past Keep.
func TestARewriteWaitsForSlackThenDropsPastKeep(t *testing.T) {
	now := time.Now()
	l := Log{Path: filepath.Join(t.TempDir(), "x.jsonl"), MaxBytes: 1, Keep: 10 * time.Hour, Slack: time.Hour}
	for _, age := range []time.Duration{10*time.Hour + 30*time.Minute, 10*time.Hour + 10*time.Minute} {
		if err := l.Append(line{At: at(now.Add(-age))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Append(line{At: at(now), N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, l.Path); len(got) != 3 {
		t.Fatalf("inside the slack: %d lines, want all 3", len(got))
	}

	l.Slack = 20 * time.Minute
	if err := l.Append(line{At: at(now), N: 2}); err != nil {
		t.Fatal(err)
	}
	got := read(t, l.Path)
	if len(got) != 2 || got[0].N != 1 || got[1].N != 2 {
		t.Fatalf("past the slack: %+v, want only the two recent lines", got)
	}
}

// Whether a rewrite is due is read from the head of the file, not all of it:
// lines are appended in time order, so the oldest are there. A file whose
// head is recent is never read whole, however large it grows — an old line
// further in waits for the head to age — and once the head has aged out the
// rewrite drops every old line, wherever it is.
func TestTheHeadDecidesWhetherARewriteIsDue(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "x.jsonl")
	var recent strings.Builder
	for i := range 4000 { // well past headBytes
		fmt.Fprintf(&recent, `{"at":%q,"n":%d}`+"\n", at(now), i)
	}
	oldTail := fmt.Sprintf(`{"at":%q,"n":-2}`+"\n", at(now.Add(-2*time.Hour)))
	if err := os.WriteFile(path, []byte(recent.String()+oldTail), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Log{Path: path, MaxBytes: 1, Keep: time.Hour, Slack: time.Minute}
	if err := l.Append(line{At: at(now), N: 4000}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); len(got) != 4002 {
		t.Fatalf("with a recent head: %d lines, want all 4002 — the file was rewritten", len(got))
	}

	oldHead := fmt.Sprintf(`{"at":%q,"n":-1}`+"\n", at(now.Add(-2*time.Hour)))
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append([]byte(oldHead), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(line{At: at(now), N: 4001}); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if len(got) != 4002 || got[0].N != 0 || got[len(got)-1].N != 4001 {
		t.Fatalf("after the rewrite: %d lines from %d to %d, want both old lines gone", len(got), got[0].N, got[len(got)-1].N)
	}
}
