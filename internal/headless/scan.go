package headless

import (
	"bytes"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Scanner stands between `claude setup-token` and the terminal. The vendor
// prints the token it minted on its screen; the scanner keeps it and passes
// everything else through with every token-shaped string withheld, so the
// secret reaches the file and never the terminal's scrollback.
//
// The vendor's screen is read the way it is written with its output on a
// pipe (2.1.296): whole frames, one after another, each line complete — no
// cursor movement and no wrapping, so a token is never split across lines.
// It may still be split across reads, so a tail that could be the start of
// one is held back until the bytes after it say where it ends.
type Scanner struct {
	out  io.Writer
	held []byte
	seen bytes.Buffer // everything passed through, withheld tokens and all

	token    string
	conflict bool // two different tokens were printed
	writeErr error
	found    chan struct{}
}

// NewScanner passes what it is written to out.
func NewScanner(out io.Writer) *Scanner { return &Scanner{out: out, found: make(chan struct{})} }

// Found is closed once the vendor has printed a token: what a mint waits for.
func (s *Scanner) Found() <-chan struct{} { return s.found }

// Withheld is what a token is replaced with on its way to the terminal.
const Withheld = "[token withheld — headroom stores it]"

// secretPrefix starts every Anthropic credential; the scanner withholds any
// string that starts with it, whatever kind of credential it is.
var secretPrefix = []byte("sk-ant-")

func secretByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// Write takes the vendor's output. It never fails the vendor: a terminal that
// stops taking output is remembered and reported by Err, and the token is
// still kept.
func (s *Scanner) Write(p []byte) (int, error) {
	s.held = append(s.held, p...)
	s.pass(false)
	return len(p), nil
}

// Close passes whatever is still held: the vendor has exited, so a token at
// the very end of its output has ended too.
func (s *Scanner) Close() error {
	s.pass(true)
	return s.writeErr
}

// Token is the token the vendor printed, "" when it printed none — or printed
// two different ones, which leaves no telling which is the account's.
func (s *Scanner) Token() string {
	if s.conflict {
		return ""
	}
	return s.token
}

// Err is the first error writing to the terminal.
func (s *Scanner) Err() error { return s.writeErr }

// pass forwards the held bytes up to the first place a token could still be
// growing, withholding every complete one.
func (s *Scanner) pass(final bool) {
	var out []byte
	rest := s.held
	for {
		i := bytes.Index(rest, secretPrefix)
		if i < 0 {
			break
		}
		end := i + len(secretPrefix)
		for end < len(rest) && secretByte(rest[end]) {
			end++
		}
		if end == len(rest) && !final {
			// The token may continue in the next read.
			out = append(out, rest[:i]...)
			rest = rest[i:]
			s.emit(out)
			s.held = append([]byte(nil), rest...)
			return
		}
		s.keep(string(rest[i:end]))
		out = append(out, rest[:i]...)
		out = append(out, Withheld...)
		rest = rest[end:]
	}
	keep := 0
	if !final {
		// A tail that is the start of the prefix waits for the next read.
		for n := min(len(secretPrefix)-1, len(rest)); n > 0; n-- {
			if bytes.HasSuffix(rest, secretPrefix[:n]) {
				keep = n
				break
			}
		}
	}
	out = append(out, rest[:len(rest)-keep]...)
	s.emit(out)
	s.held = append([]byte(nil), rest[len(rest)-keep:]...)
}

func (s *Scanner) keep(candidate string) {
	if !Valid(candidate) {
		return
	}
	if s.token != "" && s.token != candidate {
		s.conflict = true
	}
	if s.token == "" {
		close(s.found)
	}
	s.token = candidate
}

func (s *Scanner) emit(b []byte) {
	if len(b) == 0 {
		return
	}
	s.seen.Write(b)
	if s.writeErr == nil {
		_, s.writeErr = s.out.Write(b)
	}
}

// granted is the success screen's statement of how long the token lasts,
// from the expiry the vendor's token endpoint granted, rounded to days:
// "Your OAuth token (valid for 1 year):", "(valid for 30 days)", or
// "(valid for less than a day)" (2.1.296).
var granted = regexp.MustCompile(`valid for (1 year|(\d+) days?|less than a day)`)

// requested is the first screen's statement of the lifetime asked for:
// "long-lived (1-year) auth token" or "(30-day)" (2.1.296).
var requested = regexp.MustCompile(`long-lived \((1-year|(\d+)-day)\)`)

// Lifetime is how long the vendor said the token lasts: what it was granted
// when the success screen says, else what was asked for. ok is false when
// the vendor said neither, and the end is then unknown — never assumed.
func (s *Scanner) Lifetime() (time.Duration, bool) {
	text := s.seen.Bytes()
	if m := granted.FindSubmatch(text); m != nil {
		switch {
		case string(m[1]) == "1 year":
			return 365 * 24 * time.Hour, true
		case string(m[1]) == "less than a day":
			return 24 * time.Hour, true // at most
		default:
			n, _ := strconv.Atoi(string(m[2]))
			return time.Duration(n) * 24 * time.Hour, true
		}
	}
	if m := requested.FindSubmatch(text); m != nil {
		if string(m[1]) == "1-year" {
			return 365 * 24 * time.Hour, true
		}
		n, _ := strconv.Atoi(string(m[2]))
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}
