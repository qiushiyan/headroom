// Package check verifies the assumptions headroom reverse-engineered from
// Claude Code. Each FAIL means Claude Code likely changed a format on
// update. Checks go through the exact same parsers rendering uses, so the
// checker cannot drift from what rendering actually needs.
package check

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/throttle"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Exit codes: a checker that cannot distinguish "the assumption broke" from
// "I couldn't test it" is worse than useless when the dashboard misbehaves —
// it confirms the false diagnosis. Rate limiting, a dead network and a token
// mid-refresh are all reasons no evidence was gathered, not evidence against.
const (
	ExitPass         = 0
	ExitFail         = 1
	ExitInconclusive = 2
)

func Run(cfg config.Config, out io.Writer, color bool) int {
	p := render.NewPalette(color)
	fails, unknowns := 0, 0

	chk := func(ok bool, label, hint string) {
		if ok {
			fmt.Fprintf(out, "%s ok %s  %s\n", p.Grn, p.Rst, label)
			return
		}
		if hint != "" {
			hint = " — " + hint
		}
		fmt.Fprintf(out, "%sFAIL%s  %s%s\n", p.Red, p.Rst, label, hint)
		fails++
	}
	// skip records a fact that could not be tested. It never fails the run.
	skip := func(label, why string) {
		if why != "" {
			why = " — " + why
		}
		fmt.Fprintf(out, "%s ?? %s  %s%s\n", p.Yel, p.Rst, label, why)
		unknowns++
	}

	// The installed binary still carries the seams this tooling depends on:
	// the usage endpoint, config-dir routing, and the credentials service
	// name.
	needles := []string{"api/oauth/usage", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "-credentials"}
	bin := claudeBinary()
	binName, missing := "", []string(nil)
	if bin != "" {
		binName = filepath.Base(bin)
		found := searchFile(bin, needles)
		for _, n := range needles {
			if !found[n] {
				missing = append(missing, n)
			}
		}
	}
	hint := "missing: binary not found"
	if bin != "" && len(missing) > 0 {
		hint = "missing: " + strings.Join(missing, " ")
	}
	chk(bin != "" && len(missing) == 0,
		fmt.Sprintf("binary (%s): endpoint + config-dir + credential seams present", binName), hint)

	// Keychain items exist under the predicted service names, and every
	// credential blob parses through the same contract the dashboard uses.
	// (Items were written by past logins — this proves the naming
	// derivation matches what Claude Code created; the seam greps above
	// cover the binary side.) Fetches run in parallel and are validated
	// after the join, so a dead network costs one timeout, not one per
	// account.
	accts := accounts.Discover(cfg)
	now := time.Now()
	nowMS := now.UnixMilli()
	th := throttle.Load(cfg.AccountsRoot)
	client := &http.Client{Timeout: 10 * time.Second}

	const (
		modeNeverLoggedIn = iota // nothing to verify
		modeSkipped              // logged in but no usable token
		modeFetched
	)
	type apiCase struct {
		name   string // for output ("primary" reads better than the configured name)
		key    string // throttle key — the account's real name, never the label
		token  string // the token actually spent, for the 401 refresh check
		dir    string
		mode   int
		reason string
		res    usage.Result
	}
	cases := make([]*apiCase, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		name := "primary"
		if !a.IsPrimary() {
			name = a.Name
		}
		c := &apiCase{name: name, key: a.Name, dir: a.ConfigDir, mode: modeSkipped}
		cases[i] = c
		if !a.IsPrimary() {
			if _, err := os.Stat(a.MetaPath(cfg)); err != nil {
				c.mode = modeNeverLoggedIn
				continue
			}
		}
		chk(creds.HasKeychainItem(a.ConfigDir),
			fmt.Sprintf("keychain[%s]: item under predicted name", name),
			"not logged in, or naming scheme changed")

		blob, ok := creds.Parse(creds.ReadKeychain(a.ConfigDir))
		chk(ok, fmt.Sprintf("blob[%s]: parses via shared contract (accessToken present)", name), "")

		launcher := accounts.Launcher(a, accts, cfg.PrimaryName)
		switch {
		case !ok:
			c.reason = "credential blob did not parse"
		case !blob.TokenUsable(nowMS):
			// Routine: the access token ages out every ~8 hours. Nothing is
			// broken and nothing can be tested against the live endpoint.
			c.reason = fmt.Sprintf("access token stale — any %s session refreshes it", launcher)
		case !th.Eligible(a.Name, now):
			// Spending a request here to "diagnose" would deepen exactly the
			// rate limiting the user is likely running check about.
			c.reason = fmt.Sprintf("inside this account's quiet period — %s remaining",
				th.NextEligible(a.Name).Sub(now).Round(time.Second))
		default:
			c.mode = modeFetched
			c.token = blob.Token
			th.NoteAttempt(c.key, now)
		}
	}
	// Claims reach disk before any request leaves — same ordering as the
	// dashboard's, and for the same reason.
	_ = th.Save()
	for _, c := range cases {
		if c.mode != modeFetched {
			continue
		}
		wg.Add(1)
		go func(c *apiCase) {
			defer wg.Done()
			c.res = usage.Fetch(context.Background(), client, cfg.UsageURL, c.token)
		}(c)
	}
	wg.Wait()

	// Live contract, through the same parser rendering uses: 200, ≥1 row,
	// and every row's percent/timestamp well-formed. A drifted field means
	// a value is present but no longer parses — shape drift the tolerant
	// renderer would hide behind 0% / "resets ?"; an absent timestamp on an
	// untouched window passes.
	for _, c := range cases {
		switch c.mode {
		case modeNeverLoggedIn:
			continue
		case modeSkipped:
			skip(fmt.Sprintf("api[%s]: not tested", c.name), c.reason)
			continue
		}
		code := c.res.StatusCode
		label := func(extra string) string {
			codeStr := "n/a"
			if code != 0 {
				codeStr = strconv.Itoa(code)
			}
			return fmt.Sprintf("api[%s]: HTTP %s%s", c.name, codeStr, extra)
		}

		// Nothing observed → nothing to conclude.
		switch {
		case c.res.Err != nil:
			skip(label(""), "transport error — no evidence either way")
			continue
		case code == http.StatusTooManyRequests:
			th.NoteRefused(c.key, now)
			skip(label(""), "rate limited — no evidence either way")
			continue
		case code >= 500:
			skip(label(""), "vendor-side error — no evidence either way")
			continue
		case code == http.StatusUnauthorized && tokenChangedSince(c.dir, c.token):
			// Claude Code refreshed the token between our Keychain read and
			// the request, so the one we spent was already dead. That is a
			// race, not drift.
			skip(label(""), "token was refreshed mid-check — no evidence either way")
			continue
		case code != http.StatusOK:
			chk(false, label(""), "unexpected status — endpoint or auth drifted")
			continue
		}
		th.NoteSuccess(c.key, now)

		// A 200 is evidence, and now the contract is genuinely testable.
		rows, err := usage.ParseLimits(c.res.Body)
		if err != nil {
			chk(false, label(", unparseable body"), "shape drifted")
			continue
		}
		nbad := 0
		for _, r := range rows {
			if r.Drifted() {
				nbad++
			}
		}
		hint := ""
		if nbad > 0 {
			hint = fmt.Sprintf("%d malformed field(s) — shape drifted", nbad)
		}
		// Zero rows is a documented, contractual outcome — usage.ParseLimits
		// defines a nil/nil result as "this account reports no limits" — so
		// it must not be reported as drift.
		chk(nbad == 0, label(fmt.Sprintf(", %d row(s), fields well-formed", len(rows))), hint)
	}
	_ = th.Save()

	// .claude.json still records the logged-in email (dashboard labels).
	_, ok := accounts.MetaEmail(cfg.PrimaryMeta())
	chk(ok, "claude.json: .oauthAccount.emailAddress present", "")

	// The two surfaces this tool grew to depend on are checked through the
	// same parsers rendering uses, or the checker would pass while the
	// dashboard silently fell back to guesses.
	for _, a := range accts {
		name := "primary"
		if !a.IsPrimary() {
			name = a.Name
		}
		if !a.IsPrimary() {
			if _, err := os.Stat(a.MetaPath(cfg)); err != nil {
				continue
			}
		}
		switch auth.Query(a.ConfigDir).Outcome {
		case auth.OutcomeOK:
			chk(true, fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name), "")
		case auth.OutcomeUnparseable:
			chk(false, fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name),
				"output shape drifted — health now falls back to credential inference")
		default:
			skip(fmt.Sprintf("auth[%s]: not tested", name), "claude auth status unavailable")
		}

		// A cache that is present must parse; absent is ordinary and fine.
		if a.Meta.CachedUsage == nil {
			continue
		}
		rows, err := usage.ParseLimits(a.Meta.CachedUsage)
		nbad := 0
		for _, r := range rows {
			if r.Drifted() {
				nbad++
			}
		}
		chk(err == nil && nbad == 0,
			fmt.Sprintf("cache[%s]: Claude Code's cached usage parses via shared contract", name),
			"cached payload shape drifted — the offline fallback is unreadable")
	}

	fmt.Fprintln(out)
	switch {
	case fails > 0:
		fmt.Fprintf(out, "%d check(s) failed — Claude Code likely changed a format\n", fails)
		return ExitFail
	case unknowns > 0:
		fmt.Fprintf(out, "no assumption broke, but %d could not be tested — "+
			"nothing here says anything is wrong\n", unknowns)
		return ExitInconclusive
	default:
		fmt.Fprintln(out, "all checks passed")
		return ExitPass
	}
}

// tokenChangedSince reports whether the stored access token differs from the
// one a request was made with — evidence that Claude Code refreshed it while
// the request was in flight, which turns a 401 into a race rather than drift.
func tokenChangedSince(configDir, used string) bool {
	blob, ok := creds.Parse(creds.ReadKeychain(configDir))
	return !ok || blob.Token != used
}

func claudeBinary() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// searchFile streams the (large) binary through searchReader.
func searchFile(path string, needles []string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return map[string]bool{}
	}
	defer f.Close()
	return searchReader(f, needles, 4<<20)
}

// searchReader scans r in bufSize chunks, keeping an overlap of the longest
// needle so a needle split across a chunk boundary is still found.
func searchReader(r io.Reader, needles []string, bufSize int) map[string]bool {
	found := map[string]bool{}
	overlap := 0
	for _, n := range needles {
		if len(n) > overlap {
			overlap = len(n)
		}
	}
	buf := make([]byte, bufSize)
	var tail []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...)
			for _, nd := range needles {
				if !found[nd] && bytes.Contains(chunk, []byte(nd)) {
					found[nd] = true
				}
			}
			if len(found) == len(needles) {
				return found
			}
			if len(chunk) > overlap {
				tail = append(tail[:0], chunk[len(chunk)-overlap:]...)
			} else {
				tail = chunk
			}
		}
		if err != nil {
			return found
		}
	}
}
