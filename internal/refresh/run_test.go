package refresh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

const agyAnswer = `{"status":"SUCCESS","num_turns":0,"command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[` +
	`{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.75,"reset_time":"2099-01-01T00:00:00Z"},` +
	`{"id":"gemini-5h","window":"5h","remaining_fraction":0.5,"reset_time":"2099-01-01T00:00:00Z"}]}]}}}`

// stubAgy puts an agy on PATH running body, a shell script, after it has
// found the run's log path in $log. It returns the account a read is
// prepared for and the environment the read is prepared from.
func stubAgy(t *testing.T, body string) (accounts.Account, []string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nlog=\"\"\nwhile [ $# -gt 0 ]; do [ \"$1\" = \"--log-file\" ] && log=\"$2\"; shift; done\n" + body
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	scope := config.ForHome(home).Agy
	return accounts.Account{Scope: scope, Name: "primary", Readable: true}, os.Environ()
}

const signIn = `echo 'I1124 21:18:46.919352 1 server_oauth.go:221] applyAuthResult: email=me@gmail.com, authMethod=consumer, quotaProject=' > "$log"` + "\n"

func readAgy(t *testing.T, a accounts.Account, environ []string) (Result, *state.Store) {
	t.Helper()
	c, status := PrepareAgy(a, environ)
	if c == nil || status != accountstate.AttemptPending {
		t.Fatalf("PrepareAgy = %v %v", c, status)
	}
	st := state.Open(a.Scope)
	var got []Result
	for r := range Start(context.Background(), st, []*Candidate{c}, nil) {
		got = append(got, r)
	}
	if len(got) != 1 {
		t.Fatalf("results = %+v", got)
	}
	return got[0], st
}

// One read of agy lands as the spec's table says: an answer is stored with
// whose it is and replays to the same figures; a run that declined or
// printed no document leaves nothing stored and the account's figures as
// they were.
func TestAgyReadLifecycle(t *testing.T) {
	for _, c := range []struct {
		name    string
		script  string
		state   accountstate.AttemptState
		exit    int
		stored  bool
		noStart bool
	}{
		{"an answer", signIn + "cat <<'JSON'\n" + agyAnswer + "\nJSON\n", accountstate.AttemptOK, 0, true, false},
		{"an API key", "echo 'connected with an API key' >&2\nexit 2\n", accountstate.AttemptVendorFailed, 2, false, false},
		{"agy declined", signIn + "echo 'Error: authentication failed' >&2\nexit 1\n", accountstate.AttemptVendorFailed, 1, false, false},
		{"no document", "echo 'Error: authentication failed'\n", accountstate.AttemptUnparseable, 0, false, false},
		{"a document for something else", signIn + `echo '{"status":"SUCCESS","command":{"name":"credits","data":{}}}'` + "\n", accountstate.AttemptUnparseable, 0, false, false},
		{"not on PATH", "", accountstate.AttemptTransport, 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, environ := stubAgy(t, c.script)
			if c.noStart {
				t.Setenv("PATH", t.TempDir())
			}
			res, st := readAgy(t, a, environ)
			if res.Attempt.State != c.state || res.Attempt.ExitCode != c.exit || res.Attempt.HTTPCode != 0 || res.StoreErr != nil {
				t.Fatalf("attempt %+v (store %v), want %v exit %d", res.Attempt, res.StoreErr, c.state, c.exit)
			}
			stored, ok := st.Load().Observation(state.KeyFor(a), time.Now())
			if ok != c.stored {
				t.Fatalf("stored %v, want %v", ok, c.stored)
			}
			if !c.stored {
				if res.Observation != nil {
					t.Errorf("a failed read carried figures: %+v", res.Observation)
				}
				return
			}
			if res.Observation.Email != "me@gmail.com" || len(res.Observation.Rows) != 2 || res.Observation.Rows[1].Percent != 50 {
				t.Errorf("observation %+v", res.Observation)
			}
			replayed, err := usage.Parse(config.Agy, stored.Body)
			if err != nil || replayed.Email != "me@gmail.com" || len(replayed.Rows) != 2 || replayed.Rows[0].Percent != 25 {
				t.Errorf("replayed %+v %v", replayed, err)
			}
		})
	}
}

// The read answers for this machine's sign-in alone: every variable that
// would make agy answer for another credential or state dir is gone from its
// environment, the rest is passed on, and it runs from headroom's own dir.
func TestAgyReadRunsUnderItsOwnEnvironmentAndDir(t *testing.T) {
	out := filepath.Join(t.TempDir(), "seen")
	a, _ := stubAgy(t, `{ pwd; env; } > "`+out+`"`+"\n"+signIn+"cat <<'JSON'\n"+agyAnswer+"\nJSON\n")
	environ := append(os.Environ(), "GEMINI_API_KEY=key", "JETSKI_OAUTH_TOKEN=tok", "ANTIGRAVITY_GEMINI_DIR=/elsewhere", "KEEP_ME=1")
	if res, _ := readAgy(t, a, environ); res.Attempt.State != accountstate.AttemptOK {
		t.Fatalf("attempt %+v", res.Attempt)
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(seen), "\n")
	if dir, _ := filepath.EvalSymlinks(a.Scope.AccountsRoot); lines[0] != dir && lines[0] != a.Scope.AccountsRoot {
		t.Errorf("ran in %q, want the accounts root %q", lines[0], a.Scope.AccountsRoot)
	}
	for _, name := range config.Agy.EnvPolicy().Strip {
		if strings.Contains(string(seen), "\n"+name+"=") {
			t.Errorf("%s reached agy", name)
		}
	}
	if !strings.Contains(string(seen), "\nKEEP_ME=1\n") {
		t.Error("an ordinary variable was not passed on")
	}
}

// A run is bounded like a GET: one that hangs is a transport failure, and the
// round ends.
func TestAgyReadIsBounded(t *testing.T) {
	saved := runTimeout
	runTimeout = 200 * time.Millisecond
	t.Cleanup(func() { runTimeout = saved })
	a, environ := stubAgy(t, "exec sleep 10\n")
	start := time.Now()
	res, _ := readAgy(t, a, environ)
	if res.Attempt.State != accountstate.AttemptTransport {
		t.Errorf("attempt %+v, want a transport failure", res.Attempt)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the read took %v", took)
	}
}

// The log is the run's own, and gone once read.
func TestAgyReadLeavesNoLog(t *testing.T) {
	keep := filepath.Join(t.TempDir(), "log-path")
	a, environ := stubAgy(t, `echo "$log" > "`+keep+`"`+"\n"+signIn+"cat <<'JSON'\n"+agyAnswer+"\nJSON\n")
	readAgy(t, a, environ)
	path, err := os.ReadFile(keep)
	if err != nil || len(strings.TrimSpace(string(path))) == 0 {
		t.Fatalf("no log path reached agy: %v", err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(path))); !os.IsNotExist(err) {
		t.Errorf("the run's log was left behind: %v", err)
	}
}
