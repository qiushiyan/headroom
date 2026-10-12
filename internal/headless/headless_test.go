package headless

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const tok = "sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKlMnOpQrSt"

// A token is withheld from the terminal and kept, however the vendor's output
// is cut into reads: mid-prefix, mid-token, or right at its end.
func TestTheTokenIsKeptAndWithheld(t *testing.T) {
	screen := "Welcome to Claude Code v2.1.296\n\n This will guide you through long-lived (1-year) auth token setup\n" +
		" ✓ Long-lived authentication token created successfully!\n\n Your OAuth token (valid for 1 year):\n\n " + tok +
		"\n\n Store this token securely. You won't be able to see it again.\n"
	for _, size := range []int{1, 2, 3, 7, 13, 64, len(screen)} {
		var term bytes.Buffer
		s := NewScanner(&term)
		for i := 0; i < len(screen); i += size {
			s.Write([]byte(screen[i:min(i+size, len(screen))]))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if s.Token() != tok {
			t.Fatalf("reads of %d: token %q", size, s.Token())
		}
		if strings.Contains(term.String(), "sk-ant-") || !strings.Contains(term.String(), " "+Withheld+"\n") {
			t.Fatalf("reads of %d: the terminal got\n%s", size, term.String())
		}
		if want := strings.Replace(screen, tok, Withheld, 1); term.String() != want {
			t.Fatalf("reads of %d: passed through\n%q\nwant\n%q", size, term.String(), want)
		}
		if d, ok := s.Lifetime(); !ok || d != 365*24*time.Hour {
			t.Errorf("lifetime %v %v", d, ok)
		}
	}
}

// Output held back as a possible token start is released when it turns out
// not to be one, and a token at the very end of the output ends there.
func TestHeldBytesAreReleased(t *testing.T) {
	var term bytes.Buffer
	s := NewScanner(&term)
	s.Write([]byte("Paste code here if prompted > s"))
	if term.String() != "Paste code here if prompted > " {
		t.Fatalf("passed %q while a token might be starting", term.String())
	}
	s.Write([]byte("k is not a token"))
	if term.String() != "Paste code here if prompted > sk is not a token" {
		t.Fatalf("passed %q", term.String())
	}
	s.Write([]byte("\n" + tok))
	s.Close()
	if s.Token() != tok || strings.Contains(term.String(), tok) {
		t.Fatalf("token %q, terminal %q", s.Token(), term.String())
	}
}

// Any credential-shaped string is withheld, but only a vendor OAuth token is
// kept; two different tokens leave no telling which is the account's.
func TestOnlyOneOAuthTokenIsKept(t *testing.T) {
	var term bytes.Buffer
	s := NewScanner(&term)
	s.Write([]byte("key sk-ant-api03-zzzzzzzzzzzzzzzzzzzzzzzzzzzz and nothing else\n"))
	s.Close()
	if s.Token() != "" || strings.Contains(term.String(), "sk-ant-") {
		t.Fatalf("token %q terminal %q", s.Token(), term.String())
	}
	s = NewScanner(&bytes.Buffer{})
	s.Write([]byte(tok + "\n" + tok + "\n")) // the success frame drawn twice
	s.Close()
	if s.Token() != tok {
		t.Fatalf("one token printed twice: %q", s.Token())
	}
	s = NewScanner(&bytes.Buffer{})
	s.Write([]byte(tok + "\n" + strings.Replace(tok, "Ab", "Zz", 1) + "\n"))
	s.Close()
	if s.Token() != "" {
		t.Fatalf("two tokens: kept %q", s.Token())
	}
}

func TestLifetime(t *testing.T) {
	day := 24 * time.Hour
	for screen, want := range map[string]time.Duration{
		"Your OAuth token (valid for 1 year):":                         365 * day,
		"Your OAuth token (valid for 30 days):":                        30 * day,
		"Your OAuth token (valid for 1 day):":                          day,
		"Your OAuth token (valid for less than a day):":                day,
		"long-lived (1-year) auth token setup":                         365 * day,
		"long-lived (90-day) auth token … (valid for 60 days)":         60 * day,
		"long-lived (90-day) auth token setup for your Claude account": 90 * day,
	} {
		s := NewScanner(&bytes.Buffer{})
		s.Write([]byte(screen))
		s.Close()
		if got, ok := s.Lifetime(); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", screen, got, ok, want)
		}
	}
	s := NewScanner(&bytes.Buffer{})
	s.Write([]byte("the vendor reworded everything"))
	if _, ok := s.Lifetime(); ok {
		t.Error("a lifetime nobody stated")
	}
}

// The file is the user's alone: created 0600, refused when others can read
// it, never replaced when it does not decode, and a token is stored only in
// the vendor's shape.
func TestTheTokenFile(t *testing.T) {
	root := t.TempDir()
	if got, err := Read(root); err != nil || len(got) != 0 {
		t.Fatalf("absent: %v %v", got, err)
	}
	want := Token{Value: tok, MintedMS: 1, EndsMS: 2, Key: "uuid:u-a", Email: "a@x.com"}
	if err := Put(root, "a@x.com", want); err != nil {
		t.Fatal(err)
	}
	if err := Put(root, "b@x.com", Token{Value: "not-a-token"}); err == nil {
		t.Fatal("stored a value that is no token")
	}
	info, err := os.Stat(Path(root))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode().Perm(), err)
	}
	got, err := Read(root)
	if err != nil || got["a@x.com"] != want || len(got) != 1 {
		t.Fatalf("read %+v %v", got, err)
	}
	if removed, err := Remove(root, "a@x.com"); !removed || err != nil {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := Remove(root, "a@x.com"); removed || err != nil {
		t.Fatalf("remove again: %v %v", removed, err)
	}

	if err := os.Chmod(Path(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); !errors.Is(err, ErrExposed) {
		t.Fatalf("a file others can read: %v", err)
	}
	if err := Put(root, "a@x.com", want); !errors.Is(err, ErrExposed) {
		t.Fatalf("wrote into a file others can read: %v", err)
	}

	if err := os.WriteFile(Path(root), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("a torn file read as empty")
	}
	if err := Put(root, "a@x.com", want); err == nil {
		t.Fatal("a torn file was written over")
	}
	if b, _ := os.ReadFile(Path(root)); string(b) != "{torn" {
		t.Fatalf("the torn file changed: %q", b)
	}
}

func TestEnded(t *testing.T) {
	now := time.Now()
	if (Token{}).Ended(now) {
		t.Error("an unknown end read as ended")
	}
	if !(Token{EndsMS: now.Add(-time.Second).UnixMilli()}).Ended(now) || (Token{EndsMS: now.Add(time.Hour).UnixMilli()}).Ended(now) {
		t.Error("ended")
	}
}
