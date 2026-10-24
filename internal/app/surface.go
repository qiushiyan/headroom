package app

// What every surface shares with the command line above it: the streams it
// reads and writes, the statuses it can end with besides success and
// failure, which of its streams reach a terminal, and the edges a launch
// crosses. cmd/headroom translates argv into one call of a surface and its
// outcome into an exit status; nothing here reads argv or the process's own
// streams.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/qiushiyan/headroom/internal/browser"
)

// Streams are a command's input and outputs: the process's own in
// production, a test's buffers otherwise.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

// Status ends a command with an exit status after it has said everything
// itself: a dry run with no account to choose, a headless placement refused.
type Status int

func (s Status) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

// UsageError is a request the command could not carry out as asked, found
// before anything changed: a name preferred and excluded at once, a removal
// with nobody at a terminal to choose.
type UsageError struct{ Err error }

func (e UsageError) Error() string { return e.Err.Error() }
func (e UsageError) Unwrap() error { return e.Err }

func usagef(format string, args ...any) error { return UsageError{fmt.Errorf(format, args...)} }

// Terminal says which of a command's streams reach a terminal, and the
// output terminal's size. Surfaces need them differently: the board, the
// session picker and remove's picker draw on stdout and read keys from
// stdin, so they need both; mint hands the vendor stdin alone.
type Terminal struct {
	In, Out bool
	// Size is the output terminal's width and height; nil off a terminal.
	Size func() (w, h int, err error)
}

// size is the output terminal's size, or an error off one.
func (t Terminal) size() (int, int, error) {
	if t.Size == nil {
		return 0, 0, errors.New("not a terminal")
	}
	return t.Size()
}

// ExecFunc replaces this process with path, run as argv0 with args, under
// env. It returns only when it could not.
type ExecFunc func(path, argv0 string, args, env []string) error

// version is set at link time when a build names itself
// (-ldflags "-X github.com/qiushiyan/headroom/internal/app.version=<v>");
// otherwise the commit the binary was built from names it.
var version = ""

// VersionLine is `headroom version`: one line, the build's name — the commit
// it was built from, "+dirty" when the tree had uncommitted changes — and the
// commit's time. What a host install pins: the second word is the commit.
func VersionLine() string {
	rev, at, dirty := "", "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	v := version
	if v == "" {
		v = rev
		if v == "" {
			v = "unknown"
		}
		if dirty {
			v += "+dirty"
		}
	}
	line := "headroom " + v
	if at != "" {
		line += " " + at
	}
	return line
}

// Open is headroom as a vendor's $BROWSER, set by `login` and `headless mint`
// (approval.go): when args is one https URL and the opener's variables are
// set, it records the sign-in the URL belongs to, opens it in the profile
// named if one is, and reports its exit status. It runs before any
// configuration or grammar: the vendor's environment is not one headroom
// needs to accept, and a URL is no command. handled is false for any other
// invocation.
func Open(args []string, errw io.Writer) (code int, handled bool) {
	if !browser.IsOpenRequest(args) {
		return 0, false
	}
	profileDir, open := os.LookupEnv(browser.ProfileEnv)
	signin, record := os.LookupEnv(browser.SigninEnv)
	if !open && !record {
		return 0, false
	}
	return runOpen(errw, args[0], signin, profileDir, open), true
}
