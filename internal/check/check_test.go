package check

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// testReport is a report printing to a buffer, colour off, for a test that
// drives one group of assertions directly.
func testReport() (*report, *strings.Builder) {
	var out strings.Builder
	return newReport(&out, false), &out
}

// printed splits what a report printed into its ok, FAIL and ?? lines, each
// without its marker: "label" for an ok line, "label — hint" otherwise.
func printed(out *strings.Builder) (oks, fails, skips []string) {
	for l := range strings.SplitSeq(out.String(), "\n") {
		switch {
		case strings.HasPrefix(l, " ok   "):
			oks = append(oks, strings.TrimPrefix(l, " ok   "))
		case strings.HasPrefix(l, "FAIL  "):
			fails = append(fails, strings.TrimPrefix(l, "FAIL  "))
		case strings.HasPrefix(l, " ??   "):
			skips = append(skips, strings.TrimPrefix(l, " ??   "))
		}
	}
	return
}

// hasLine reports a line carrying every token of want, the tokens separated
// by " … ".
func hasLine(lines []string, want string) bool {
	return slices.ContainsFunc(lines, func(l string) bool {
		for tok := range strings.SplitSeq(want, " … ") {
			if !strings.Contains(l, tok) {
				return false
			}
		}
		return true
	})
}

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
// a damaged `.reserved` and an inherited CLAUDE_CONFIG_DIR even when the state
// document was written by a newer headroom and the state audit
// short-circuits. Every failure here is headroom's own, never vendor drift;
// TestRunVerdicts exercises the composition with fixture processes and HTTP.
func TestCheckRouting(t *testing.T) {
	mkdir := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		vendor   config.Vendor // "" is Claude Code
		setup    func(t *testing.T, cfg *config.Scope)
		environ  []string
		wantOK   []string
		wantFail []string // own failures, each one line
		absent   string
	}{
		// A clean environment earns no env line.
		{name: "clean", absent: "env:"},
		// An inherited variable — present-but-empty included, which is
		// unverified vendor territory rather than the verified absent state —
		// earns the ok-with-detail line.
		{name: "inherited", environ: []string{"CLAUDE_CONFIG_DIR=/leak"}, wantOK: []string{`env: inherited CLAUDE_CONFIG_DIR is neutralized by managed launches ("/leak")`}},
		{name: "present but empty", environ: []string{"CLAUDE_CONFIG_DIR="}, wantOK: []string{`env: inherited CLAUDE_CONFIG_DIR is neutralized by managed launches ("")`}},
		// A non-empty relative value is the stale-wrapper incident's
		// signature: unmanaged claude in that shell runs as the primary while
		// writing state beside the cwd.
		{name: "relative ambient", environ: []string{"CLAUDE_CONFIG_DIR=yan@planlab.ai"}, wantFail: []string{"env: inherited CLAUDE_CONFIG_DIR is relative"}},
		// A stranded `<dir>.lock` is skipped by discovery; check is where it
		// gets named — an ok-with-detail line, never a failure.
		{name: "lock debris", setup: func(t *testing.T, cfg *config.Scope) { mkdir(t, filepath.Join(cfg.AccountsRoot, "a@x.com.lock")) },
			wantOK: []string{"root: a@x.com.lock is vendor lock debris"}},
		// The topology assertion runs per extra account through the same
		// verifier launch refuses with, so the gate and the report cannot
		// disagree.
		{name: "forked projects/", setup: func(t *testing.T, cfg *config.Scope) {
			mkdir(t, filepath.Join(cfg.AccountsRoot, "yan@planlab.ai", "projects")) // a real dir: the fork
			mkdir(t, cfg.StoreDir())
		}, wantFail: []string{"topology[yan@planlab.ai]: projects/ resolves to the canonical store"}},
		// A relocated home re-points what headroom observes but not what a
		// primary launch would use, which the vendor resolves against the real
		// home: launch refuses the primary, for either vendor, and check says
		// so.
		{name: "relocated home", setup: func(t *testing.T, cfg *config.Scope) { cfg.PrimaryRelocated = true },
			wantFail: []string{"home: HEADROOM_HOME re-points the primary headroom describes … `claude`"}},
		{name: "relocated home, Codex", vendor: config.Codex, setup: func(t *testing.T, cfg *config.Scope) { cfg.PrimaryRelocated = true },
			wantFail: []string{"home: HEADROOM_HOME re-points the primary headroom describes … `codex`"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := claudeScope(home, "qiushi")
			if tc.vendor == config.Codex {
				cfg = config.ForHome(home).Codex
				cfg.PrimaryName = "qiushi"
			}
			if tc.setup != nil {
				tc.setup(t, &cfg)
			}
			r, out := testReport()
			checkRouting(accounts.Discover(cfg), append([]string{"HOME=" + home}, tc.environ...), r)
			oks, fails, _ := printed(out)
			for _, w := range tc.wantOK {
				if !hasLine(oks, w) {
					t.Errorf("no ok line %q in:\n%s", w, out)
				}
			}
			for _, w := range tc.wantFail {
				if !hasLine(fails, w) {
					t.Errorf("no FAIL line %q in:\n%s", w, out)
				}
			}
			if len(fails) != len(tc.wantFail) || r.ownFails != len(fails) {
				t.Errorf("want %d own failure(s), got %d (%d own):\n%s", len(tc.wantFail), len(fails), r.ownFails, out)
			}
			if tc.absent != "" && strings.Contains(out.String(), tc.absent) {
				t.Errorf("unexpected %q line:\n%s", tc.absent, out)
			}
		})
	}
}

// Every account's cleanup sweep prunes the one shared store, so the store
// keeps the shortest period any sharing account names. Once a second account
// shares the store, every sharing account must read one settings.json and
// that file must set the period: separate files drift apart, and an unset
// value is whatever Claude Code's default is in the running version. Both
// fail as own-state; an unreadable setting is untested; an account off the
// store is not judged (its topology line fails instead); a primary alone
// shares nothing, so its default is reported, not judged.
func TestCheckRetention(t *testing.T) {
	const shared = `{"cleanupPeriodDays": 365}`
	const oneFile = "every account sharing the session store reads one settings.json"
	for _, tc := range []struct {
		name               string
		primary, a, forked string // settings.json bodies; "" = no file, "link" = the primary's, "-" = no account a
		wantOK             []string
		wantFail           []string
		wantSkip           string
	}{
		{name: "one shared file", primary: shared, a: "link", wantOK: []string{oneFile, "after 365 days"}},
		{name: "account on the default", primary: shared, wantFail: []string{oneFile, "primary 365, a@x.com 30 (unset)"}},
		{name: "one shared file, period unset", primary: "{}", a: "link", wantOK: []string{oneFile}, wantFail: []string{"cleanupPeriodDays unset (primary, a@x.com)"}},
		{name: "no settings anywhere", wantFail: []string{oneFile, "cleanupPeriodDays unset (primary, a@x.com)"}},
		{name: "separate files that agree", primary: shared, a: shared, wantOK: []string{"after 365 days"}, wantFail: []string{oneFile + " — primary reads "}},
		{name: "primary alone on the default", a: "-", wantOK: []string{"after 30 days (cleanupPeriodDays unset"}},
		{name: "unreadable is untested", primary: "{", a: "link", wantOK: []string{oneFile}, wantSkip: "retention[a@x.com]"},
		{name: "rejected value is untested", primary: `{"cleanupPeriodDays": 0}`, a: "link", wantOK: []string{oneFile}, wantSkip: "retention[a@x.com]"},
		{name: "forked account not judged", primary: shared, a: "link", forked: `{"cleanupPeriodDays": 7}`, wantOK: []string{oneFile, "after 365 days"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := claudeScope(home, "qiushi")
			write := func(path, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			primarySettings := filepath.Join(cfg.PrimaryDir(), "settings.json")
			if tc.primary != "" {
				write(primarySettings, tc.primary)
			}
			extra := filepath.Join(cfg.AccountsRoot, "a@x.com")
			if tc.a != "-" {
				write(filepath.Join(extra, ".claude.json"), `{}`)
				if err := os.Symlink(cfg.StoreDir(), filepath.Join(extra, "projects")); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.a {
			case "", "-":
			case "link":
				if err := os.Symlink(primarySettings, filepath.Join(extra, "settings.json")); err != nil {
					t.Fatal(err)
				}
			default:
				write(filepath.Join(extra, "settings.json"), tc.a)
			}
			if tc.forked != "" {
				forked := filepath.Join(cfg.AccountsRoot, "b@x.com")
				write(filepath.Join(forked, "projects", "-p", "s.jsonl"), "")
				write(filepath.Join(forked, "settings.json"), tc.forked)
			}

			r, out := testReport()
			checkRetention(accounts.Discover(cfg), r)
			oks, fails, skips := printed(out)
			for _, w := range tc.wantOK {
				if !hasLine(oks, w) {
					t.Errorf("want ok %q, got ok=%v fail=%v", w, oks, fails)
				}
			}
			for _, w := range tc.wantFail {
				if !hasLine(fails, w) {
					t.Errorf("want fail %q, got ok=%v fail=%v", w, oks, fails)
				}
			}
			if len(fails) != len(tc.wantFail) || r.ownFails != len(fails) {
				t.Errorf("want %d own fail(s), got %v (%d own)", len(tc.wantFail), fails, r.ownFails)
			}
			if (tc.wantSkip != "" && !hasLine(skips, tc.wantSkip)) || (tc.wantSkip == "" && len(skips) != 0) {
				t.Errorf("want skip %q, got %v", tc.wantSkip, skips)
			}
		})
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
			r, out := testReport()
			if tc.codex {
				r.group(config.Codex, "")
			}
			r.request("a", tc.result, time.Now())
			_, _, skips := printed(out)
			if tc.wantLabel != "" && !hasLine(skips, tc.wantLabel) {
				t.Errorf("inconclusive lines=%v, want %q", skips, tc.wantLabel)
			}
			if vendor := r.fails - r.ownFails; vendor != tc.vendor || r.ownFails != tc.own || r.unknowns != tc.unknown {
				t.Fatalf("verdicts: vendor=%d own=%d inconclusive=%d", vendor, r.ownFails, r.unknowns)
			}
		})
	}
}

// Real command, credential and HTTP boundaries use local fixtures so this
// pins exit status and user-facing evidence together without personal state.
// The closing line says whose files a failure is against: a corrupt state
// file or a config dir the launch seam refuses is headroom's own, and must
// never read as the vendor having changed a format; output the vendor's own
// health command no longer shapes, and a cache it wrote that can no longer be
// attributed, are the vendor's. Every row also proves checkRetention ran —
// even when the state file is newer and its audit short-circuits.
func TestRunVerdicts(t *testing.T) {
	const own = "all against headroom's own files"
	const drift = "Claude Code likely changed a format"
	type fixture struct {
		cfg   *config.Scope
		bin   string
		write func(path, body string, mode os.FileMode)
	}
	for _, tc := range []struct {
		name      string
		status    int
		setup     func(t *testing.T, f fixture)
		noRequest bool // the endpoint must not be asked
		want      int
		fail      string // a FAIL line carrying this
		phrase    string // the output carries this
		absent    string // and not this
	}{
		{name: "pass", status: 200, want: ExitPass, phrase: "all checks passed"},
		{name: "refused", status: 429, want: ExitInconclusive, phrase: "rate limited"},
		{name: "state failure", status: 200, setup: func(t *testing.T, f fixture) {
			// Something is in state.json's place and cannot be read, so no
			// claim can be recorded: headroom's own state, not the vendor's.
			if err := os.MkdirAll(filepath.Join(f.cfg.AccountsRoot, "state.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, noRequest: true, want: ExitFail, fail: "state[primary]: request bookkeeping", phrase: own, absent: "likely changed a format"},
		{name: "newer state", status: 200, setup: func(t *testing.T, f fixture) {
			f.write(filepath.Join(f.cfg.AccountsRoot, "state.json"), `{"version":999}`, 0600)
		}, want: ExitInconclusive, phrase: "api[primary]: not tested"},
		{name: "auth output drifted", status: 200, setup: func(t *testing.T, f fixture) {
			f.write(filepath.Join(f.bin, "claude"), "#!/bin/sh\n# api/oauth/usage CLAUDE_CONFIG_DIR CLAUDE_SECURESTORAGE_CONFIG_DIR -credentials\necho '{\"other\":1}'\n", 0755)
		}, want: ExitFail, fail: "auth[primary]: claude auth status parses via shared contract", phrase: drift},
		// A relative config dir is a construction headroom refuses, so the
		// probe is never built: that failure is the launch seam's, not drift.
		{name: "auth probe unbuildable", status: 200, setup: func(t *testing.T, f fixture) {
			home := filepath.Dir(f.cfg.AccountsRoot)
			t.Chdir(home)
			f.cfg.AccountsRoot = "accounts"
			dir := filepath.Join(home, "accounts", "a@x.com")
			f.write(filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u-a"}}`, 0600)
			if err := os.Symlink(f.cfg.StoreDir(), filepath.Join(dir, "projects")); err != nil {
				t.Fatal(err)
			}
		}, want: ExitFail, fail: "auth[a@x.com]: probe environment could not be constructed", phrase: own, absent: "likely changed a format"},
		// Present but unattributable: the offline fallback silently went away.
		{name: "cache rejected", status: 200, setup: func(t *testing.T, f fixture) {
			f.write(f.cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary","accountUuid":"fixture"},
			  "cachedUsageUtilization":{"fetchedAtMs":1,"accountUuid":"a previous login","utilization":{"limits":[]}}}`, 0600)
		}, want: ExitFail, fail: "cache[primary]: Claude Code's cached usage parses via shared contract", phrase: drift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg, write := soundClaudeTree(t, home)
			if tc.setup != nil {
				tc.setup(t, fixture{&cfg, os.Getenv("PATH"), write})
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.noRequest {
					t.Error("unclaimable request reached endpoint")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"limits":[]}`))
			}))
			defer srv.Close()
			cfg.UsageURL = srv.URL
			var out strings.Builder
			code := Run(config.Config{Claude: cfg}, &out, false, nil)
			_, fails, _ := printed(&out)
			text := out.String()
			if code != tc.want || !strings.Contains(text, tc.phrase) || !strings.Contains(text, "retention:") ||
				(tc.fail != "" && !hasLine(fails, tc.fail)) || (tc.absent != "" && strings.Contains(text, tc.absent)) {
				t.Fatalf("exit=%d want=%d, FAIL %q, carrying %q and not %q:\n%s", code, tc.want, tc.fail, tc.phrase, tc.absent, text)
			}
		})
	}
}

func TestRunPrimaryCredentialSource(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		status           int
		wantCode         int
		wantLine         string
	}{
		{"file only", `{"claudeAiOauth":{"accessToken":"fixture"}}`, 200, ExitPass, "credential[primary]: .credentials.json (no keychain item)"},
		// The 401 re-check reads the primary's real dir, as the first read
		// did: the token there is unchanged, so the rejection is drift — not
		// an unreadable credential that would leave it untested.
		{"file only, token rejected", `{"claudeAiOauth":{"accessToken":"fixture"}}`, 401, ExitFail, "api[primary]: HTTP 401 — unexpected status"},
		{"neither", "", 200, ExitFail, "credential[primary]: no credential source — not logged in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg, write := soundClaudeTree(t, home)
			write(filepath.Join(os.Getenv("PATH"), "security"), "#!/bin/sh\nexit 1\n", 0755)
			if tc.credential != "" {
				write(filepath.Join(cfg.PrimaryDir(), ".credentials.json"), tc.credential, 0600)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"limits":[]}`))
			}))
			defer srv.Close()
			cfg.UsageURL = srv.URL
			var out strings.Builder
			code := Run(config.Config{Claude: cfg}, &out, false, nil)
			if code != tc.wantCode || !strings.Contains(out.String(), tc.wantLine) {
				t.Fatalf("exit=%d want=%d, missing %q:\n%s", code, tc.wantCode, tc.wantLine, out.String())
			}
		})
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
	write(filepath.Join(cfg.StoreDir(), "-project", "session.jsonl"), fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"session\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"fixture\"}}\n", project), 0600)
	return cfg, write
}

// The registry status is the one field automatic placement reads beyond
// liveness. A running session without one is drift and fails; a word this
// binary has not met is not drift, and is reported as untested.
func TestCheckRegistryStatus(t *testing.T) {
	run := func(entries ...sessions.RegistryEntry) (fails, oks, skips []string) {
		r, out := testReport()
		checkRegistryStatus(entries, r)
		oks, fails, skips = printed(out)
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

// The session records' drift assertions. History with lines but no parseable
// claim means attribution silently fell back to placing a named session like
// a new one. A registry record that does not parse leaves liveness
// incomplete: busy load goes uncounted, and removal refuses what it cannot
// prove closed.
func TestCheckSessionStore(t *testing.T) {
	for _, tc := range []struct {
		name, history, registry string
		wantOK                  string
		wantFail                []string
	}{
		{name: "history that parses", history: `{"sessionId":"s","timestamp":1}` + "\n", wantOK: "history[primary]"},
		{name: "history with lines but no parseable claim", history: `{"display":"a prompt"}` + "\n", wantFail: []string{"history[primary]: prompt records carry sessionId + timestamp"}},
		{name: "a registry record that does not parse", registry: `{"sessionId":"s","pid":`, wantFail: []string{"registry: live-session records carry sessionId + pid + startedAt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := claudeScope(home, "qiushi")
			write := func(path, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.history != "" {
				write(filepath.Join(cfg.PrimaryDir(), "history.jsonl"), tc.history)
			}
			if tc.registry != "" {
				write(filepath.Join(cfg.PrimaryDir(), "sessions", "7.json"), tc.registry)
			}
			r, out := testReport()
			checkSessionStore(accounts.Discover(cfg).Accounts, nil, r)
			oks, fails, _ := printed(out)
			if tc.wantOK != "" && !hasLine(oks, tc.wantOK) {
				t.Errorf("no ok line %q in:\n%s", tc.wantOK, out)
			}
			if len(fails) != len(tc.wantFail) {
				t.Errorf("want FAIL %q alone, got:\n%s", tc.wantFail, out)
			}
			for _, w := range tc.wantFail {
				if !hasLine(fails, w) {
					t.Errorf("no FAIL %q in:\n%s", w, out)
				}
			}
		})
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
			code := Run(config.Config{Claude: cfg}, &out, false, tc.probe)
			failed := false
			for line := range strings.SplitSeq(out.String(), "\n") {
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

// A login, history or registry linked between dirs makes two dirs one login:
// headroom's own state, and a failure.
func TestCheckLoginsFailsOnALinkedLogin(t *testing.T) {
	cfg := config.ForHome(t.TempDir()).Claude
	for _, dir := range []string{filepath.Join(cfg.AccountsRoot, "a@x.com"), filepath.Join(cfg.AccountsRoot, "b@x.com")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func() (passed, failed []string, own int) {
		r, out := testReport()
		checkLogins(accounts.Discover(cfg), r)
		passed, failed, _ = printed(out)
		return passed, failed, r.ownFails
	}
	if passed, failed, _ := run(); len(failed) != 0 || !hasLine(passed, "logins: every account dir keeps its own login") {
		t.Fatalf("clean dirs: passed %v, failed %v", passed, failed)
	}
	if err := os.Symlink(filepath.Join(cfg.AccountsRoot, "a@x.com", ".credentials.json"), filepath.Join(cfg.AccountsRoot, "b@x.com", ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	_, failed, own := run()
	if !hasLine(failed, "logins: … .credentials.json is a link") || own != len(failed) {
		t.Errorf("a linked login: failed %v, %d of them headroom's own", failed, own)
	}
}

// `.reserved` is checked only where it exists, through the reader a launch
// uses. A damaged file refuses automatic launches, and a stale row refuses
// nothing but is worth hearing about.
func TestCheckReservations(t *testing.T) {
	for _, c := range []struct {
		name string
		body func(dir string) string
		fail string
	}{
		{"none", nil, ""},
		{"sound", func(dir string) string { return "qiushi\t" + dir + "\n" }, ""},
		{"damaged", func(string) string { return "not a row\n" }, ".reserved line 1"},
		{"a directory gone", func(dir string) string { return "qiushi\t" + dir + "/gone\n" }, "can be launched only by name"},
		{"an account gone", func(dir string) string { return "nobody@x.com\t" + dir + "\n" }, "not a discovered account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := claudeScope(home, "qiushi")
			if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.body != nil {
				if err := os.WriteFile(cfg.ReservedFile(), []byte(c.body(home)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, out := testReport()
			checkReservations(accounts.Discover(cfg), r)
			oks, fails, _ := printed(out)
			switch {
			case c.body == nil && len(oks)+len(fails) != 0:
				t.Errorf("no file, and it said: %s", out)
			case c.body != nil && c.fail == "" && len(oks) != 1:
				t.Errorf("want an ok line: %s", out)
			case c.fail != "" && (r.ownFails != 1 || !strings.Contains(out.String(), c.fail)):
				t.Errorf("want one own failure naming %q: %s", c.fail, out)
			}
		})
	}
}
