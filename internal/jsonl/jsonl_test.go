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

// The check before a rewrite reads the head of the file, not all of it: a
// large file whose head is recent is left alone however often it is appended
// to, and one whose head has aged out is rewritten.
func TestTheHeadDecidesWhetherARewriteIsDue(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	var b strings.Builder
	for i := range 4000 { // well past headBytes
		fmt.Fprintf(&b, `{"at":%q,"n":%d}`+"\n", at(now), i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Log{Path: path, MaxBytes: 1, Keep: time.Hour, Slack: time.Minute}
	if l.due(now) {
		t.Error("a recent head was found due")
	}
	old := fmt.Sprintf(`{"at":%q,"n":-1}`+"\n", at(now.Add(-2*time.Hour)))
	if err := os.WriteFile(path, []byte(old+b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.due(now) {
		t.Error("an aged-out head was not found due")
	}
	if err := l.Append(line{At: at(now), N: 4000}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); len(got) != 4001 || got[0].N != 0 {
		t.Fatalf("after the rewrite: %d lines starting at %d", len(got), got[0].N)
	}
}
