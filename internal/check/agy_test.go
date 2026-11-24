package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// agyCheckFixture is a present Antigravity with an agy on PATH running body
// once it has the run's log path in $log.
func agyCheckFixture(t *testing.T, body string) config.Scope {
	t.Helper()
	home := t.TempDir()
	scope := config.ForHome(home).Agy
	scope.Present = true
	bin := filepath.Join(home, "bin")
	for _, d := range []string{scope.PrimaryDir(), bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nlog=\"\"\nwhile [ $# -gt 0 ]; do [ \"$1\" = \"--log-file\" ] && log=\"$2\"; shift; done\n" + body
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return scope
}

const agyCheckAnswer = `echo '{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":0.6,"reset_time":"2099-01-01T00:00:00Z"}]}]}}}'` + "\n"

const agyCheckSignIn = `echo 'I1124 1 server_oauth.go:221] applyAuthResult: email=me@gmail.com, authMethod=consumer' > "$log"` + "\n"

func runAgyCheck(scope config.Scope, environ []string) *codexRun {
	rep, out := testReport()
	rep.group(config.Agy, "")
	checkAgy(scope, environ, rep)
	r := &codexRun{fails: rep.fails, own: rep.ownFails, skips: rep.unknowns}
	oks, fails, skips := printed(out)
	for _, l := range oks {
		r.lines = append(r.lines, "ok "+l)
	}
	for _, l := range fails {
		r.lines = append(r.lines, "FAIL "+l)
	}
	for _, l := range skips {
		r.lines = append(r.lines, "?? "+l)
	}
	return r
}

// The one read is the evidence: it parses through the board's parser, and
// its log names whose sign-in answered.
func TestAgyCheckReadsThroughTheBoardsParser(t *testing.T) {
	scope := agyCheckFixture(t, agyCheckSignIn+agyCheckAnswer)
	r := runAgyCheck(scope, append(os.Environ(), "GEMINI_API_KEY=key"))
	for _, want := range []string{
		"ok binary: agy on PATH",
		"ok env: inherited GEMINI_API_KEY (value not shown) is stripped from headroom's reads",
		"ok read[primary]: agy -p /usage, 1 row(s), fields well-formed",
		"ok identity[primary]: the read's log names its sign-in",
		"ok state: every section readable",
	} {
		if !r.has(want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(r.lines, "\n"))
		}
	}
	if r.fails != 0 || r.skips != 0 {
		t.Errorf("fails %d skips %d:\n%s", r.fails, r.skips, strings.Join(r.lines, "\n"))
	}
}

func TestAgyCheckVerdicts(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		fails, skips     int
	}{
		{"a log naming nobody", agyCheckAnswer, "FAIL identity[primary]: the read's log names its sign-in", 1, 0},
		{"an answer that drifted", agyCheckSignIn + `echo '{"status":"SUCCESS","command":{"name":"usage"}}'` + "\n", "FAIL read[primary]: agy -p /usage, unparseable body", 1, 0},
		// Why agy declined is agy's to say: no evidence of drift either way.
		{"agy declined", "exit 1\n", "?? read[primary]: agy -p /usage — agy exited 1", 0, 1},
		{"an API key", "exit 2\n", "?? read[primary]: agy -p /usage — agy is using an API key", 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := runAgyCheck(agyCheckFixture(t, c.body), os.Environ())
			if !r.has(c.want) || r.fails != c.fails || r.skips != c.skips {
				t.Errorf("want %q, fails %d skips %d; got fails %d skips %d:\n%s", c.want, c.fails, c.skips, r.fails, r.skips, strings.Join(r.lines, "\n"))
			}
		})
	}
	t.Run("no agy on PATH", func(t *testing.T) {
		scope := agyCheckFixture(t, "")
		t.Setenv("PATH", t.TempDir())
		r := runAgyCheck(scope, os.Environ())
		if !r.has("FAIL binary: agy on PATH") || r.fails != 1 {
			t.Errorf("%s", strings.Join(r.lines, "\n"))
		}
	})
}
