package peers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// Two machines run builds installed at different times: a document of
// another schema, or for another vendor, or cut short of its readings list,
// is refused by name, never read around. Of an account given twice, the
// newer reading stands wherever it sits in the list.
func TestDecodeRefusesWhatItDoesNotRead(t *testing.T) {
	for _, c := range []struct{ doc, want string }{
		{`not json`, "not a readings document"},
		{`{"schema":2,"vendor":"claude","readings":[]}`, "schema 2"},
		{`{"schema":1,"vendor":"codex","readings":[]}`, `answered for "codex"`},
		{`{"schema":1,"vendor":"claude"}`, "no readings list"},
	} {
		if _, err := Decode(config.Claude, []byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.doc, err, c.want)
		}
	}
	got, err := Decode(config.Claude, []byte(`{"schema":1,"vendor":"claude","readings":[
		{"account":"uuid:a","name":"a@x","fetched_at_ms":9,"body":{"limits":[],"n":9}},
		{"account":"uuid:a","name":"a@x","fetched_at_ms":5,"body":{"limits":[],"n":5}}]}`))
	if err != nil || got["uuid:a"].FetchedAtMS != 9 || !strings.Contains(string(got["uuid:a"].Body), `"n":9`) {
		t.Errorf("decoded %+v, %v; want the newer of a repeated account", got, err)
	}
	if got, err := Decode(config.Claude, []byte(`{"schema":1,"vendor":"claude","readings":[]}`)); err != nil || len(got) != 0 {
		t.Errorf("an empty list = %v, %v; want an answer with no readings", got, err)
	}
}

// stubSSH puts an ssh on PATH that records its arguments, prints out and
// errOut and exits code — never the real one.
func stubSSH(t *testing.T, out, errOut string, code int) string {
	t.Helper()
	bin, args := t.TempDir(), filepath.Join(t.TempDir(), "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nprintf '%%s' %q\nprintf '%%s' %q >&2\nexit %d\n", args, out, errOut, code)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return args
}

// The pull never prompts, gives up connecting quickly, and runs the other
// machine's installed headroom for the vendor asked.
func TestSSHAsksWithoutPrompting(t *testing.T) {
	args := stubSSH(t, `{"schema":1}`, "", 0)
	out, err := SSH(context.Background(), "me@mini2", config.Codex)
	if err != nil || string(out) != `{"schema":1}` {
		t.Fatalf("SSH = %q, %v", out, err)
	}
	b, _ := os.ReadFile(args)
	got := string(b)
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=3", "-T", "me@mini2\n", "readings --vendor codex", "$HOME/.local/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh arguments %q lack %q", got, want)
		}
	}
}

// Why a pull failed is said in terms of what to do: the machine was not
// reached, has no headroom, or has one from before this command — that last
// only on the refusal such a build prints, never on any exit 2.
func TestSSHSaysWhyThereIsNoAnswer(t *testing.T) {
	for _, c := range []struct {
		out, errOut string
		code        int
		want        string
	}{
		{"", "ssh: connect to host mini2 port 22: Operation timed out\n", 255, "ssh could not reach it: ssh: connect to host mini2 port 22: Operation timed out"},
		{"", "zsh:1: command not found: headroom\n", 127, "no headroom"},
		{"", "headroom: error: unexpected argument readings\nUsage: headroom [<command>] [flags]\nRun \"headroom --help\" for details.\n", 2, "no readings command"},
		{"", "sh: bad substitution\n", 2, "exited 2: sh: bad substitution"},
		{"", "boom\n", 1, "exited 1: boom"},
	} {
		stubSSH(t, c.out, c.errOut, c.code)
		if _, err := SSH(context.Background(), "mini2", config.Claude); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("exit %d: err = %v, want %q", c.code, err, c.want)
		}
	}
}

// What a pull reads is bounded in size and in time, whatever the other end
// does: an answer past the cap is refused even when it is valid JSON, and a
// child left holding the output pipe does not keep the ask past its bound.
func TestSSHIsBounded(t *testing.T) {
	bin := t.TempDir()
	write := func(script string) {
		if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	write("#!/bin/sh\nhead -c 5000000 /dev/zero | tr '\\0' ' '\nprintf '{\"schema\":1,\"vendor\":\"claude\",\"readings\":[]}'\n")
	if _, err := SSH(context.Background(), "mini2", config.Claude); err == nil || !strings.Contains(err.Error(), "ran past") {
		t.Errorf("an answer past the cap: err = %v, want it refused", err)
	}

	write("#!/bin/sh\nsleep 30 &\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := SSH(ctx, "mini2", config.Claude)
	if d := time.Since(start); d > 3*time.Second || err == nil {
		t.Errorf("a child holding the pipe: returned after %s with %v, want an error within the bound", d, err)
	}
}

// A pull records the other machine's failure as its outcome and reports only
// this machine's own bookkeeping as an error; one cut short by the caller's
// end records nothing, so it is never told as the machine not answering.
func TestPullRecordsOutcomesNotTheCallersEnd(t *testing.T) {
	scope := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
	st := state.Open(scope)
	refuse := func(context.Context, string, config.Vendor) ([]byte, error) {
		return nil, errors.New("ssh could not reach it: timed out")
	}
	if answered, err := Pull(context.Background(), st, "mini2", refuse); answered || err != nil {
		t.Errorf("a refused pull = %v, %v; want no answer and no error of this machine's", answered, err)
	}
	if s, _ := st.Load().PeerStatus("mini2"); s.Error != "ssh could not reach it: timed out" {
		t.Errorf("status = %+v, want the failure recorded", s)
	}

	scope.AccountsRoot = t.TempDir()
	st = state.Open(scope)
	ctx, cancel := context.WithCancel(context.Background())
	hang := func(ctx context.Context, _ string, _ config.Vendor) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	Pull(ctx, st, "mini2", hang)
	if s, _ := st.Load().PeerStatus("mini2"); s.Error != "" || s.FailedMS != 0 {
		t.Errorf("a pull cut short by its caller was recorded as a failure: %+v", s)
	}
}

// A pull bounds its ask itself: a caller with no deadline of its own still
// hands the asker one, no later than askWait.
func TestAPullBoundsItsAsk(t *testing.T) {
	scope := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
	var deadline time.Time
	var bounded bool
	ask := func(ctx context.Context, _ string, _ config.Vendor) ([]byte, error) {
		deadline, bounded = ctx.Deadline()
		return []byte(`{"schema":1,"vendor":"claude","readings":[]}`), nil
	}
	start := time.Now()
	if answered, err := Pull(context.Background(), state.Open(scope), "mini2", ask); !answered || err != nil {
		t.Fatalf("pull = %v, %v", answered, err)
	}
	if !bounded || deadline.After(start.Add(askWait+time.Second)) {
		t.Errorf("the ask's deadline = %v (bounded %v), want one within %v", deadline, bounded, askWait)
	}
}
