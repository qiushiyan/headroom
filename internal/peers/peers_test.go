package peers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// Two machines run builds installed at different times: a document of
// another schema, or for another vendor, is refused by name, never read
// around.
func TestDecodeRefusesWhatItDoesNotRead(t *testing.T) {
	for _, c := range []struct{ doc, want string }{
		{`not json`, "not a readings document"},
		{`{"schema":2,"vendor":"claude","readings":[]}`, "schema 2"},
		{`{"schema":1,"vendor":"codex","readings":[]}`, `answered for "codex"`},
	} {
		if _, err := Decode(config.Claude, []byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.doc, err, c.want)
		}
	}
	got, err := Decode(config.Claude, []byte(`{"schema":1,"vendor":"claude","readings":[
		{"account":"uuid:a","name":"a@x","fetched_at_ms":5,"body":{"limits":[]}},
		{"account":"uuid:a","name":"a@x","fetched_at_ms":9,"body":{"limits":[]}}]}`))
	if err != nil || len(got) != 1 || got["uuid:a"].FetchedAtMS != 9 {
		t.Errorf("decoded %+v, %v; want the newest of a repeated account", got, err)
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
// reached, has no headroom, or has one from before this command.
func TestSSHSaysWhyThereIsNoAnswer(t *testing.T) {
	for _, c := range []struct {
		out, errOut string
		code        int
		want        string
	}{
		{"", "ssh: connect to host mini2 port 22: Operation timed out\n", 255, "ssh could not reach it: ssh: connect to host mini2 port 22: Operation timed out"},
		{"", "zsh:1: command not found: headroom\n", 127, "no headroom"},
		{"", "headroom: error: unexpected argument readings\n", 2, "no readings command"},
		{"", "boom\n", 1, "exited 1: boom"},
	} {
		stubSSH(t, c.out, c.errOut, c.code)
		if _, err := SSH(context.Background(), "mini2", config.Claude); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("exit %d: err = %v, want %q", c.code, err, c.want)
		}
	}
}
