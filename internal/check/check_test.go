package check

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
)

// The overlap carry is the subtle part: a needle split across a chunk
// boundary must still be found, for every split position and even when the
// needle is longer than the read buffer.
func TestSearchReader(t *testing.T) {
	needles := []string{"NEEDLE", "api/oauth"}
	cases := []struct {
		name    string
		content string
		bufSize int
		want    map[string]bool
	}{
		{"inside one chunk", "xxNEEDLExx api/oauth xx", 64,
			map[string]bool{"NEEDLE": true, "api/oauth": true}},
		{"split across boundary", "xxxxxxNEEDLExxxx", 8, // boundary at E|DLE
			map[string]bool{"NEEDLE": true}},
		{"needle longer than buffer", "xxNEEDLExx", 3,
			map[string]bool{"NEEDLE": true}},
		{"missing", "nothing here", 8, map[string]bool{}},
		{"at the very end", "xxxxxxxxxxNEEDLE", 8, map[string]bool{"NEEDLE": true}},
	}
	for _, c := range cases {
		got := searchReader(strings.NewReader(c.content), needles, c.bufSize)
		for n, want := range c.want {
			if got[n] != want {
				t.Errorf("%s: found[%q] = %v, want %v", c.name, n, got[n], want)
			}
		}
		if len(c.want) == 0 && len(got) != 0 {
			t.Errorf("%s: unexpected finds %v", c.name, got)
		}
	}

	// Exhaustive: split "NEEDLE" at every boundary a 4-byte buffer produces
	// for every prefix length.
	for pad := range 12 {
		content := strings.Repeat("x", pad) + "NEEDLE" + strings.Repeat("y", 3)
		got := searchReader(strings.NewReader(content), []string{"NEEDLE"}, 4)
		if !got["NEEDLE"] {
			t.Errorf("pad %d: needle lost at chunk boundary", pad)
		}
	}

	// A reader that returns data and EOF in the same call (io.Reader allows
	// it) must not drop the final chunk.
	r := iotest.DataErrReader(strings.NewReader("xxxxxxNEEDLE"))
	if got := searchReader(r, []string{"NEEDLE"}, 8); !got["NEEDLE"] {
		t.Error("data+EOF read dropped the final chunk")
	}
}

// checkRouting is deliberately independent of state.json — it must report
// corrupt `.current` and an inherited CLAUDE_CONFIG_DIR even when the state
// document was written by a newer headroom and the state audit
// short-circuits. These cases cover the routing decisions directly;
// TestRunVerdicts exercises their composition with fixture processes and HTTP.
func TestCheckRouting(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "qiushi")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)

	run := func(environ []string) (ownFails, envLines []string) {
		chk := func(ok bool, label, hint string) {
			if ok {
				envLines = append(envLines, label)
			}
		}
		own := func(ok bool, label, hint string) {
			if !ok {
				ownFails = append(ownFails, label)
			}
		}
		checkRouting(accts, environ, chk, own)
		return
	}

	// Absent .current: the documented default — no failure, and a clean
	// environment earns no env line.
	if fails, lines := run([]string{"HOME=" + home}); len(fails) != 0 || len(lines) != 0 {
		t.Errorf("clean state: ownFails=%v envLines=%v", fails, lines)
	}

	// Corrupt (empty) .current: an own-state FAIL — headroom's file, never
	// vendor drift.
	if err := os.WriteFile(cfg.CurrentFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if fails, _ := run([]string{"HOME=" + home}); len(fails) != 1 {
		t.Errorf("empty .current: want one own FAIL, got %v", fails)
	}
	if err := os.Remove(cfg.CurrentFile()); err != nil {
		t.Fatal(err)
	}

	// An inherited variable — present-but-empty included, which is
	// unverified vendor territory rather than the verified absent state —
	// earns the ok-with-detail line.
	if _, lines := run([]string{"CLAUDE_CONFIG_DIR=/leak"}); len(lines) != 1 {
		t.Errorf("inherited value: want env line, got %v", lines)
	}
	if _, lines := run([]string{"CLAUDE_CONFIG_DIR="}); len(lines) != 1 {
		t.Errorf("present-but-empty: want env line, got %v", lines)
	}
}

// A stranded `<dir>.lock` in the accounts root is skipped by discovery; the
// checker is where it gets named — an ok-with-detail line, never a failure.
func TestCheckRoutingNamesLockDebris(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "qiushi")
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, "a@x.com.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)
	if len(accts.Accounts) != 1 {
		t.Fatalf("debris must not be discovered: %v", accts)
	}
	var noted bool
	chk := func(ok bool, label, hint string) {
		if ok && strings.Contains(label, "a@x.com.lock") {
			noted = true
		}
	}
	own := func(ok bool, label, hint string) {
		if !ok {
			t.Errorf("debris must not fail check: %s", label)
		}
	}
	checkRouting(accts, []string{"HOME=" + home}, chk, own)
	if !noted {
		t.Error("stranded lock dir was not named by check")
	}
}

// A non-empty relative inherited value is the stale-wrapper incident's
// signature — unmanaged claude in that shell runs as the primary while
// writing state beside the cwd — and fails as own-state, never as drift.
func TestCheckRoutingFailsRelativeAmbientDir(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "qiushi")
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	chk := func(bool, string, string) {}
	checkRouting(accts, []string{"CLAUDE_CONFIG_DIR=yan@planlab.ai"}, chk, own)
	found := false
	for _, l := range ownFails {
		if strings.Contains(l, "relative") {
			found = true
		}
	}
	if !found {
		t.Errorf("relative ambient dir did not fail: %v", ownFails)
	}
}

// The topology assertion runs per extra account through the same verifier
// launch refuses with, so the gate and the report cannot disagree.
func TestCheckRoutingAssertsTopology(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "qiushi")
	extra := filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	if err := os.MkdirAll(filepath.Join(extra, "projects"), 0o755); err != nil { // real dir: the fork
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	checkRouting(accts, nil, func(bool, string, string) {}, own)
	found := false
	for _, l := range ownFails {
		if strings.Contains(l, "topology[yan@planlab.ai]") {
			found = true
		}
	}
	if !found {
		t.Errorf("forked topology not reported: %v", ownFails)
	}
}

func TestRequestVerdictsSeparateStateFailureAndVendorEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		result               refresh.Result
		vendor, own, unknown int
		wantLabel            string
		codex                bool
	}{
		{"busy claim", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptStateUnavailable}, StoreErr: state.ErrBusy}, 0, 0, 2, "", false},
		{"transport", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptTransport}}, 0, 0, 1, "api[a]: HTTP n/a", false},
		{"degraded claim", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptStateUnavailable}, StoreErr: state.ErrCorrupt}, 0, 1, 1, "", false},
		{"received but not stored", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptUnparseable, HTTPCode: 200}, StoreErr: state.ErrCorrupt}, 1, 1, 0, "", false},
		{"malformed response", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptUnparseable, HTTPCode: 200}}, 1, 0, 0, "", false},
		{"token changed", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenChanged}, 0, 0, 1, "", false},
		{"token unknown", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnknown}, 0, 0, 1, "", false},
		{"token unchanged", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnchanged}, 1, 0, 0, "", false},
		// Codex recovers from a 401 by refreshing: never drift, whatever the re-read says.
		{"codex token unchanged", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnchanged}, 0, 0, 1, "api[a]: HTTP 401", true},
		{"codex token unknown", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnknown}, 0, 0, 1, "", true},
		// A completion that could not be recorded is headroom's own failure,
		// independent of the endpoint's verdict — a Codex 401 included.
		{"codex 401 and not stored", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, StoreErr: state.ErrCorrupt}, 0, 1, 1, "", true},
		{"codex other 4xx is still drift", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 403}}, 1, 0, 0, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vendor, own, unknown := 0, 0, 0
			var labels []string
			v := config.Claude
			if tc.codex {
				v = config.Codex
			}
			reportRequest(v, "a", tc.result, time.Now(), func(ok bool, _, _ string) {
				if !ok {
					vendor++
				}
			}, func(ok bool, _, _ string) {
				if !ok {
					own++
				}
			}, func(label, _ string) { unknown++; labels = append(labels, label) })
			if tc.wantLabel != "" && !strings.Contains(strings.Join(labels, "\n"), tc.wantLabel) {
				t.Errorf("inconclusive labels=%v, want %q", labels, tc.wantLabel)
			}
			if vendor != tc.vendor || own != tc.own || unknown != tc.unknown {
				t.Fatalf("verdicts: vendor=%d own=%d inconclusive=%d", vendor, own, unknown)
			}
		})
	}
}

// Real command, credential and HTTP boundaries use local fixtures so this
// pins exit status and user-facing evidence together without personal state.
func TestRunVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		brokenOwners bool
		want         int
		phrase       string
	}{
		{"pass", 200, false, ExitPass, "passed"},
		{"refused", 429, false, ExitInconclusive, "rate limited"},
		{"state failure", 200, true, ExitFail, "api[primary]: not tested"},
		{"newer state", 200, false, ExitInconclusive, "api[primary]: not tested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg, write := soundClaudeTree(t, home)
			if tc.name == "newer state" {
				write(filepath.Join(cfg.AccountsRoot, "state.json"), `{"version":999}`, 0600)
			}
			if tc.brokenOwners {
				write(filepath.Join(cfg.AccountsRoot, ".owners"), "broken", 0600)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.brokenOwners {
					t.Error("unclaimable request reached endpoint")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"limits":[]}`))
			}))
			defer srv.Close()
			cfg.UsageURL = srv.URL
			var out strings.Builder
			code := Run(config.Config{Home: home, Claude: cfg}, &out, false, nil)
			if code != tc.want || !strings.Contains(out.String(), tc.phrase) {
				t.Fatalf("exit=%d want=%d\n%s", code, tc.want, out.String())
			}
		})
	}
}

func TestRunPrimaryCredentialSource(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		wantCode         int
		wantLine         string
	}{
		{"file only", `{"claudeAiOauth":{"accessToken":"fixture"}}`, ExitPass, "credential[primary]: .credentials.json (no keychain item)"},
		{"neither", "", ExitFail, "credential[primary]: no credential source — not logged in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg, write := soundClaudeTree(t, home)
			write(filepath.Join(os.Getenv("PATH"), "security"), "#!/bin/sh\nexit 1\n", 0755)
			if tc.credential != "" {
				write(filepath.Join(cfg.PrimaryDir(), ".credentials.json"), tc.credential, 0600)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"limits":[]}`))
			}))
			defer srv.Close()
			cfg.UsageURL = srv.URL
			var out strings.Builder
			code := Run(config.Config{Home: home, Claude: cfg}, &out, false, nil)
			if code != tc.wantCode || !strings.Contains(out.String(), tc.wantLine) {
				t.Fatalf("exit=%d want=%d, missing %q:\n%s", code, tc.wantCode, tc.wantLine, out.String())
			}
		})
	}
}

func TestRereadCredentialFilePrimary(t *testing.T) {
	primaryDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(primaryDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"refreshed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if token, ok := rereadCredential(primaryDir)(""); !ok || token != "refreshed" {
		t.Errorf("401 re-check got %q, %v", token, ok)
	}
}

// soundClaudeTree builds a Claude Code fixture every assertion passes on —
// stub claude and security on PATH, a logged-in primary, a registry, history
// and one transcript — and hands back its scope and a file writer. The usage
// URL is the caller's to set.
func soundClaudeTree(t *testing.T, home string) (config.Scope, func(path, body string, mode os.FileMode)) {
	t.Helper()
	bin := t.TempDir()
	cfg := claudeScope(home, "primary")
	cfg.AccountsRoot = filepath.Join(home, "accounts")
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "claude"), "#!/bin/sh\n# api/oauth/usage CLAUDE_CONFIG_DIR CLAUDE_SECURESTORAGE_CONFIG_DIR -credentials\necho '{\"loggedIn\":true}'\n", 0755)
	write(filepath.Join(bin, "security"), "#!/bin/sh\necho '{\"claudeAiOauth\":{\"accessToken\":\"fixture\"}}'\n", 0755)
	t.Setenv("PATH", bin)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	write(cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary","accountUuid":"fixture"}}`, 0600)
	write(filepath.Join(cfg.PrimaryDir(), "sessions", "1.json"), `{"sessionId":"session","pid":1,"startedAt":1,"status":"idle"}`, 0600)
	write(filepath.Join(cfg.PrimaryDir(), "history.jsonl"), `{"sessionId":"session","timestamp":1}`+"\n", 0600)
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(cfg.StoreDir(), sessions.Munge(project), "session.jsonl"), fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"session\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"fixture\"}}\n", project), 0600)
	return cfg, write
}

// The registry status is the one field automatic placement reads beyond
// liveness. A running session without one is drift and fails; a word this
// binary has not met is not drift, and is reported as untested.
func TestCheckRegistryStatus(t *testing.T) {
	run := func(entries ...sessions.RegistryEntry) (fails, oks, skips []string) {
		chk := func(ok bool, label, hint string) {
			if ok {
				oks = append(oks, label)
			} else {
				fails = append(fails, label+" — "+hint)
			}
		}
		skip := func(label, why string) { skips = append(skips, label+" — "+why) }
		checkRegistryStatus(entries, chk, skip)
		return
	}
	entry := func(status string, state tag.State) sessions.RegistryEntry {
		return sessions.RegistryEntry{Account: "a", SessionID: "s", PID: 1, OK: true, Status: status, StatusState: state}
	}

	if fails, oks, skips := run(); len(fails)+len(oks)+len(skips) != 0 {
		t.Errorf("no running sessions is nothing to assert: %v %v %v", fails, oks, skips)
	}
	if fails, oks, skips := run(entry("busy", tag.OK), entry("idle", tag.OK), entry("shell", tag.OK)); len(fails) != 0 || len(oks) != 1 || len(skips) != 0 {
		t.Errorf("the known vocabulary: fails %v oks %v skips %v", fails, oks, skips)
	}
	// The field gone from every running session is drift.
	if fails, _, _ := run(entry("", tag.None), entry("", tag.Bad)); len(fails) != 1 {
		t.Errorf("no running session carries a status: %v", fails)
	}
	// Gone from some is more likely a stale record under a recycled pid.
	if fails, _, skips := run(entry("busy", tag.OK), entry("", tag.None), entry("", tag.Bad)); len(fails) != 0 || len(skips) != 1 || !strings.Contains(skips[0], "2 of 3") {
		t.Errorf("some without a status: fails %v skips %v", fails, skips)
	}
	fails, oks, skips := run(entry("busy", tag.OK), entry("thinking", tag.OK), entry("compacting", tag.OK))
	if len(fails) != 0 || len(oks) != 1 || len(skips) != 1 ||
		!strings.Contains(skips[0], `"compacting", "thinking"`) {
		t.Errorf("an unfamiliar status is untested, not drift: fails %v skips %v", fails, skips)
	}
}

// The status assertion is made about sessions verified live — pid and start
// time, the evidence routing uses — and through the command itself, so the
// probe `check` is handed is the probe that decides it. A pid that answers but
// was started at another time is another process: it must not fail the check,
// and without a probe nothing is known to be live.
func TestRunJudgesRegistryStatusOverVerifiedLiveSessions(t *testing.T) {
	const started = 1_785_700_003_000
	const label = "registry: running sessions say what they are doing (status)"
	live := func(int) (int64, error) { return started / 1000, nil }
	recycled := func(int) (int64, error) { return started/1000 + 86_400, nil }
	for _, tc := range []struct {
		name   string
		status string
		probe  sessions.PIDProbe
		want   int
	}{
		{"live with no status fails", "", live, ExitFail},
		{"live with a status passes", `,"status":"busy"`, live, ExitPass},
		{"a recycled pid is not a running session", "", recycled, ExitPass},
		{"no probe, nothing known live", "", nil, ExitPass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg, write := soundClaudeTree(t, home)
			sdir := filepath.Join(cfg.PrimaryDir(), "sessions")
			if err := os.MkdirAll(sdir, 0o755); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(sdir, "7.json"), fmt.Sprintf(`{"sessionId":"s7","pid":7,"startedAt":%d%s}`, started, tc.status), 0600)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"limits":[]}`))
			}))
			defer srv.Close()
			cfg.UsageURL = srv.URL
			var out strings.Builder
			code := Run(config.Config{Home: home, Claude: cfg}, &out, false, tc.probe)
			failed := false
			for _, line := range strings.Split(out.String(), "\n") {
				if strings.Contains(line, label) && strings.Contains(line, "FAIL") {
					failed = true
				}
			}
			if code != tc.want || failed != (tc.want == ExitFail) {
				t.Fatalf("exit=%d want=%d, status line failed=%v\n%s", code, tc.want, failed, out.String())
			}
		})
	}
}

// Automatic placement is a routing state, not a missing one: check passes it,
// and still fails the file when the word is ambiguous.
func TestCheckRoutingUnderAuto(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "qiushi")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.CurrentFile(), []byte("auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() (passed, failed []string) {
		own := func(ok bool, label, hint string) {
			if ok {
				passed = append(passed, label)
			} else {
				failed = append(failed, label+" — "+hint)
			}
		}
		checkRouting(accounts.Discover(cfg), []string{"HOME=" + home}, func(bool, string, string) {}, own)
		return
	}
	passed, failed := run()
	if len(failed) != 0 || len(passed) == 0 || !strings.Contains(passed[0], ".current says auto") {
		t.Fatalf("auto: passed %v failed %v", passed, failed)
	}
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, "auto"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, failed = run()
	reserved := false
	for _, f := range failed {
		reserved = reserved || (strings.Contains(f, "current:") && strings.Contains(f, "reserved"))
	}
	if !reserved {
		t.Fatalf("auto beside an account named auto: %v", failed)
	}
}
