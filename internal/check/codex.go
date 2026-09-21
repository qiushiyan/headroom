package check

// The Codex group: the reverse-engineered Codex facts headroom stands on,
// verified with the same PASS / FAIL / INCONCLUSIVE discipline and through
// the same readers the board uses (codexauth, usage.Parse, refresh).
//
// Two things differ from Claude Code's group, both on purpose. A 401 is never
// a FAIL: Codex itself recovers from one by reloading and refreshing, so a
// rejected token is not evidence that anything drifted. And there is a
// credential-source probe: headroom reads only Codex's file credential store,
// so a home where `codex login status` and auth.json disagree is said out
// loud rather than rendered as a plain "not logged in".

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

// loginVerdict is what one `codex login status` run established.
type loginVerdict int

const (
	loginUnavailable loginVerdict = iota // could not run, timed out, or said nothing recognizable
	loginYes
	loginNo
)

// loginStatusTimeout bounds one probe. The command is local (~17ms measured).
const loginStatusTimeout = 5 * time.Second

// probeLogin runs `codex login status` against one home. home "" is the
// primary, selected by CODEX_HOME's absence. The environment is the launch
// constructor's, so an inherited CODEX_HOME or credential variable cannot
// answer for the wrong login. "Not logged in" arrives on stderr with exit 1
// (measured 0.155.0), so both streams are read and the words — not the exit
// code — tell "not logged in" from a command that failed.
func probeLogin(bin, home string, environ []string) loginVerdict {
	tgt, err := launch.For(config.Codex, home)
	if err != nil {
		return loginUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "login", "status")
	cmd.Env = tgt.Env(environ)
	out, _ := cmd.CombinedOutput()
	return classifyLogin(string(out))
}

func classifyLogin(out string) loginVerdict {
	text := strings.ToLower(out)
	switch {
	case strings.Contains(text, "not logged in"):
		return loginNo
	case strings.Contains(text, "logged in"):
		return loginYes
	default:
		return loginUnavailable
	}
}

func checkCodex(scope config.Scope, environ []string,
	chk, own func(bool, string, string), skip func(string, string)) {

	// The installed binary still carries the seams: the usage endpoint, the
	// account header, home routing, and the ambient credential override the
	// launch constructor strips.
	needles := []string{"wham/usage", "ChatGPT-Account-Id", "CODEX_HOME", "CODEX_ACCESS_TOKEN"}
	bin, resolved := codexBinary()
	if bin == "" {
		chk(false, "binary: codex on PATH", "not found")
	} else {
		found := searchFile(resolved, needles)
		var missing []string
		for _, n := range needles {
			if !found[n] {
				missing = append(missing, n)
			}
		}
		label := fmt.Sprintf("binary (%s): endpoint + account header + home + credential seams present", filepath.Base(resolved))
		if len(missing) > 0 && isLauncherScript(resolved) {
			// An npm install puts a small script on PATH and the native binary
			// somewhere it resolves at run time; there is nothing to search.
			skip("binary: seams not searched", filepath.Base(resolved)+" is a launcher script, not the native binary")
		} else {
			chk(len(missing) == 0, label, "missing: "+strings.Join(missing, " "))
		}
	}

	set := accounts.Discover(scope)
	accts := set.Accounts
	now := time.Now()
	name := func(a accounts.Account) string {
		if a.IsPrimary() {
			return "primary"
		}
		return a.Name
	}

	// The auth document, through the reader the board uses.
	requests := make([]*refresh.Candidate, len(accts))
	for i, a := range accts {
		label := fmt.Sprintf("auth[%s]: auth.json parses via shared contract (ChatGPT login, identity decodable)", name(a))
		switch a.Auth.State {
		case codexauth.Absent:
			// Ordinary: never logged in here. The credential-source probe
			// below is what notices a login kept somewhere else.
			continue
		case codexauth.Unreadable:
			chk(false, label, "unreadable, or no longer a JSON object")
		case codexauth.NoTokens:
			chk(false, label, "a ChatGPT login without a tokens.access_token string — shape drifted")
		case codexauth.IdentityUnknown:
			chk(false, label, "tokens.account_id or the id token's chatgpt_user_id could not be decoded — the request budget cannot be identified")
		case codexauth.OtherMode:
			chk(true, fmt.Sprintf("auth[%s]: auth_mode %q has no subscription window — no usage is read", name(a), a.Auth.Mode), "")
		case codexauth.OK:
			chk(true, label, "")
			candidate, eligibility := refresh.PrepareCodex(a, now)
			requests[i] = candidate
			if candidate == nil && eligibility == accountstate.AttemptTokenStale {
				skip(fmt.Sprintf("api[%s]: not tested", name(a)),
					fmt.Sprintf("access token stale — any %s session refreshes it", accounts.Launcher(a)))
			}
		}
	}

	st := state.Open(scope)
	results := make([]refresh.Result, len(accts))
	for r := range refresh.Start(context.Background(), st, requests, rereadCodex) {
		results[r.Index] = r
	}
	for i, candidate := range requests {
		if candidate != nil {
			reportCodexRequest(name(accts[i]), results[i], now, chk, own, skip)
		}
	}

	// The isolation and credential-source probes. One `codex login status`
	// per home, in parallel, each under its own CODEX_HOME.
	if bin != "" {
		empty, err := os.MkdirTemp("", "headroom-codex-isolation-")
		verdicts := make([]loginVerdict, len(accts))
		isolation := loginUnavailable
		var wg sync.WaitGroup
		for i, a := range accts {
			wg.Add(1)
			go func(i int, home string) {
				defer wg.Done()
				verdicts[i] = probeLogin(bin, home, environ)
			}(i, a.ConfigDir)
		}
		if err == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				isolation = probeLogin(bin, empty, environ)
			}()
		}
		wg.Wait()
		if err == nil {
			os.RemoveAll(empty)
		}

		// CODEX_HOME must isolate a login: an empty home answers "not logged
		// in" whatever the primary holds. Everything headroom does for Codex
		// rests on this, so anything else fails loudly.
		switch isolation {
		case loginNo:
			chk(true, "isolation: CODEX_HOME set to an empty dir answers not logged in", "")
		case loginYes:
			chk(false, "isolation: CODEX_HOME set to an empty dir answers not logged in",
				"an empty home reports a login — CODEX_HOME no longer selects the credentials, or they moved out of the home (keyring?)")
		default:
			skip("isolation: not tested", "codex login status gave no recognizable answer")
		}

		for i, a := range accts {
			label := fmt.Sprintf("credentials[%s]: codex login status agrees with auth.json", name(a))
			fileLogin := a.Auth.State == codexauth.OK || a.Auth.State == codexauth.IdentityUnknown
			switch {
			case verdicts[i] == loginUnavailable:
				skip(fmt.Sprintf("credentials[%s]: not tested", name(a)), "codex login status gave no recognizable answer")
			case verdicts[i] == loginYes && a.Auth.State == codexauth.Absent:
				// Logged in, and not from the file headroom reads: the keyring
				// credential store, most likely. Not drift and not provable —
				// the board shows this home as not logged in.
				skip(label, "logged in with no auth.json — credential source undetermined (cli_auth_credentials_store = \"keyring\"?); headroom reads only the file store, so the board shows this home as not logged in")
			case verdicts[i] == loginNo && fileLogin:
				chk(false, label, "auth.json holds a ChatGPT login the vendor does not accept — the document's meaning drifted")
			default:
				chk(true, label, "")
			}
		}
	}

	checkOwnState(st.Load(), chk, own, skip)
	checkRouting(set, environ, chk, own)
}

// rereadCodex is Codex's credential reader for the 401 re-check.
func rereadCodex(home string) (string, bool) {
	snap := codexauth.Read(home)
	return snap.AccessToken, snap.AccessToken != ""
}

// reportCodexRequest is reportRequest with Codex's 401 rule: any 401 is
// inconclusive. Claude Code's rule — an unchanged token rejected is drift —
// does not carry over, because Codex recovers from a 401 by refreshing.
func reportCodexRequest(name string, r refresh.Result, now time.Time, chk, own func(bool, string, string), skip func(string, string)) {
	if r.Attempt.State == accountstate.AttemptHTTP && r.Attempt.HTTPCode == http.StatusUnauthorized {
		detail := "access token rejected — any Codex session refreshes it; no evidence either way"
		if r.TokenAfter401 == refresh.TokenChanged {
			detail = "token was refreshed mid-check — no evidence either way"
		}
		skip(fmt.Sprintf("api[%s]: HTTP 401", name), detail)
		return
	}
	reportRequest(name, r, now, chk, own, skip)
}

func codexBinary() (path, resolved string) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", ""
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return path, r
	}
	return path, path
}

// isLauncherScript reports a `#!` file: a wrapper, not the native binary.
func isLauncherScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 2)
	n, _ := f.Read(head)
	return n == 2 && string(head) == "#!"
}
