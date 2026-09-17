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
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
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
		checkRouting(cfg, accts, environ, chk, own)
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
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, "a@x.com.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)
	if len(accts) != 1 {
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
	checkRouting(cfg, accts, []string{"HOME=" + home}, chk, own)
	if !noted {
		t.Error("stranded lock dir was not named by check")
	}
}

// A non-empty relative inherited value is the stale-wrapper incident's
// signature — unmanaged claude in that shell runs as the primary while
// writing state beside the cwd — and fails as own-state, never as drift.
func TestCheckRoutingFailsRelativeAmbientDir(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "qiushi"}
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	chk := func(bool, string, string) {}
	checkRouting(cfg, accts, []string{"CLAUDE_CONFIG_DIR=yan@planlab.ai"}, chk, own)
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
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "qiushi"}
	extra := filepath.Join(cfg.AccountsRoot, "yan@planlab.ai")
	if err := os.MkdirAll(filepath.Join(extra, "projects"), 0o755); err != nil { // real dir: the fork
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.ProjectsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	accts := accounts.Discover(cfg)

	var ownFails []string
	own := func(ok bool, label, hint string) {
		if !ok {
			ownFails = append(ownFails, label)
		}
	}
	checkRouting(cfg, accts, nil, func(bool, string, string) {}, own)
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
	}{
		{"busy claim", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptStateUnavailable}, StoreErr: state.ErrBusy}, 0, 0, 2, ""},
		{"transport", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptTransport}}, 0, 0, 1, "api[a]: HTTP n/a"},
		{"degraded claim", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptStateUnavailable}, StoreErr: state.ErrCorrupt}, 0, 1, 1, ""},
		{"received but not stored", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptUnparseable, HTTPCode: 200}, StoreErr: state.ErrCorrupt}, 1, 1, 0, ""},
		{"malformed response", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptUnparseable, HTTPCode: 200}}, 1, 0, 0, ""},
		{"token changed", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenChanged}, 0, 0, 1, ""},
		{"token unknown", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnknown}, 0, 0, 1, ""},
		{"token unchanged", refresh.Result{Attempt: accountstate.Attempt{State: accountstate.AttemptHTTP, HTTPCode: 401}, TokenAfter401: refresh.TokenUnchanged}, 1, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vendor, own, unknown := 0, 0, 0
			var labels []string
			reportRequest("a", tc.result, time.Now(), func(ok bool, _, _ string) {
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
			bin := t.TempDir()
			cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, "accounts"), PrimaryName: "primary"}
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
			write(filepath.Join(cfg.PrimaryDir(), "sessions", "1.json"), `{"sessionId":"session","pid":1,"startedAt":1}`, 0600)
			write(filepath.Join(cfg.PrimaryDir(), "history.jsonl"), `{"sessionId":"session","timestamp":1}`+"\n", 0600)
			project := filepath.Join(home, "project")
			if err := os.MkdirAll(project, 0755); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(cfg.ProjectsDir(), sessions.Munge(project), "session.jsonl"), fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"session\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"fixture\"}}\n", project), 0600)
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
			code := Run(cfg, &out, false)
			if code != tc.want || !strings.Contains(out.String(), tc.phrase) {
				t.Fatalf("exit=%d want=%d\n%s", code, tc.want, out.String())
			}
		})
	}
}
