package app

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/render"
)

// ran is a surface's outcome as the command line reports it: the exit status
// cmd/headroom maps its error to, and both streams, an error's text last on
// stderr where the command line prints it.
type ran struct {
	code           int
	stdout, stderr string
}

func finish(err error, out, errw *bytes.Buffer) ran {
	code := 0
	if s, ok := errors.AsType[Status](err); ok {
		code = int(s)
	} else if err != nil {
		code = 1
		if _, ok := errors.AsType[UsageError](err); ok {
			code = 2
		}
		errw.WriteString("headroom: error: " + err.Error() + "\n")
	}
	return ran{code: code, stdout: out.String(), stderr: errw.String()}
}

// call runs a surface over a test's buffers.
func call(f func(std Streams) error) ran {
	var out, errw bytes.Buffer
	return finish(f(Streams{In: bytes.NewReader(nil), Out: &out, Err: &errw}), &out, &errw)
}

// execRecord stands in for the exec that replaces this process, and records
// what it was asked to start.
type execRecord struct {
	called       bool
	binary       string
	args, env    []string
	err          error // what the exec returns
	atExec       func()
	path, argv0s string
}

func (r *execRecord) exec(path, binary string, args, env []string) error {
	r.called, r.path, r.binary, r.args, r.env = true, path, binary, args, env
	if r.atExec != nil {
		r.atExec()
	}
	return r.err
}

// quietPlace is a placement's edges with nothing behind them: no credential
// reads anything, no process is running, no refresh is left behind.
func quietPlace() launchop.Deps {
	return launchop.Deps{
		Creds:   func(accounts.Account) string { return "" },
		Probe:   func(int) (int64, error) { return 0, os.ErrNotExist },
		Refresh: func(config.Scope) error { return nil },
	}
}

// launchOptions reads the launch flags these tests spell as argv. The
// command line's grammar is cmd/headroom's, pinned there; this translates
// the few spellings the tests use, and everything after `--` reaches the
// vendor.
func launchOptions(t *testing.T, args ...string) LaunchOptions {
	t.Helper()
	var o LaunchOptions
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--auto":
			o.Auto = true
		case "--last":
			o.Last = true
		case "--remember":
			o.Remember = true
		case "--dry-run":
			o.DryRun = true
		case "--account":
			i++
			o.Account = args[i]
		case "--":
			o.Args = slices.Clone(args[i+1:])
			return o
		default:
			t.Fatalf("launch flag %q is not one these tests spell", a)
		}
	}
	return o
}

// boardDeps are the board's edges over the fixture: the live round, whose
// credential and health readers the fixture stubs on PATH, and the fixture's
// placement edges for the marks.
func (f autoFixture) boardDeps() BoardDeps {
	return BoardDeps{Round: LiveRound(true), Place: f.edges}
}

// picker is a session picker over set under routing, its exec recorded and
// what it says after the picker closes written to errw.
func (f autoFixture) picker(set accounts.Set, routing accounts.Routing) (*resumeUI, *execRecord, *bytes.Buffer) {
	rec, errw := &execRecord{}, &bytes.Buffer{}
	ui := &resumeUI{beforeLaunch: func() {}, st: f.st, set: set, routing: routing, place: f.edges, exec: rec.exec, errw: errw}
	return ui, rec, errw
}

// rearm gives a picker a fresh exec record for its next commit.
func rearm(ui *resumeUI) *execRecord {
	rec := &execRecord{}
	ui.exec = rec.exec
	return rec
}

// The two terminal facts are each surface's to read. The board opens its
// picker only with a terminal on both streams — it reads keys on one and
// redraws the other — and on either alone prints one frame. A mint asks only
// stdin, where the vendor asks for the approval: a terminal on stdout alone
// does not let it start.
func TestEachSurfaceReadsTheTerminalFactsItNeeds(t *testing.T) {
	f := boardFixture(t, "a@x.com")
	for _, term := range []Terminal{{In: true}, {Out: true, Size: func() (int, int, error) { return 100, 40, nil }}} {
		got := call(func(std Streams) error {
			return Board(std, term, []config.Scope{f.cfg}, render.LayoutBlocks, f.boardDeps())
		})
		if got.code != 0 || !strings.Contains(got.stdout, "a@x.com") {
			t.Errorf("board under %+v: exit %d\n%s%s", term, got.code, got.stdout, got.stderr)
		}
	}
	got := call(func(std Streams) error { return HeadlessMint(std, Terminal{Out: true}, f.cfg, "a@x.com") })
	if got.code != 1 || !strings.Contains(got.stderr, "needs a terminal") {
		t.Errorf("mint with a terminal on stdout alone: exit %d\n%s", got.code, got.stderr)
	}
}
