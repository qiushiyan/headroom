package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
