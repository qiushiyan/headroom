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
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tag"
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
		case code == http.StatusUnauthorized && recheckToken(c.dir, c.token) == tokenChanged:
			// Claude Code refreshed the token between our Keychain read and
			// the request, so the one we spent was already dead. That is a
			// race, not drift.
			skip(label(""), "token was refreshed mid-check — no evidence either way")
			continue
		case code == http.StatusUnauthorized && recheckToken(c.dir, c.token) == tokenUncertain:
			skip(label(""), "credential unreadable on re-check — cannot tell a refresh race from drift")
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
				"output shape drifted — account health can no longer be established")
		default:
			skip(fmt.Sprintf("auth[%s]: not tested", name), "claude auth status unavailable")
		}

		// A cache that is present must be usable. Absent is ordinary — Claude
		// Code writes one only once it has fetched — but present-and-rejected
		// means the offline fallback silently disappeared, which is exactly
		// the drift this check exists to catch.
		cacheLabel := fmt.Sprintf("cache[%s]: Claude Code's cached usage parses via shared contract", name)
		switch a.Meta.CacheState {
		case tag.None:
			continue
		case tag.Bad:
			chk(false, cacheLabel,
				"cached payload present but its account identity or fetch time is missing — fallback lost")
			continue
		}
		rows, err := usage.ParseLimits(a.Meta.CachedUsage)
		nbad := 0
		for _, r := range rows {
			if r.Drifted() {
				nbad++
			}
		}
		chk(err == nil && nbad == 0, cacheLabel,
			"cached payload shape drifted — the offline fallback is unreadable")
	}

	checkSessionStore(cfg, accts, chk, skip)

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

// checkSessionStore verifies the contracts the resume surface stands on:
// prompt history attributes sessions to accounts, transcripts carry their own
// titles and a verifiable cwd within the tail budget, the live-session
// registry parses (it is what stops `dd` from deleting an open transcript),
// and headroom's own .owners file is readable. Shapes, never census numbers —
// counts are wrong the day after they're written down.
func checkSessionStore(cfg config.Config, accts []accounts.Account,
	chk func(bool, string, string), skip func(string, string)) {

	// history.jsonl: the attribution source. Absent or empty is a fresh
	// account, not drift; present with lines but zero parseable claims means
	// the shape moved and affinity is silently routing everything to the
	// current account.
	for _, a := range accts {
		name := "primary"
		if !a.IsPrimary() {
			name = a.Name
		}
		f, err := os.Open(filepath.Join(a.Dir(cfg), "history.jsonl"))
		if err != nil {
			continue
		}
		h := sessions.ParseHistory(f)
		f.Close()
		if h.Lines == 0 {
			continue
		}
		chk(len(h.Newest) > 0,
			fmt.Sprintf("history[%s]: prompt records carry sessionId + timestamp", name),
			"no line parses — session→account attribution is gone")
	}

	// The live-session registry: when claim files exist, they must parse,
	// or every open session silently reads as deletable.
	regFiles, regOK := 0, 0
	for _, a := range accts {
		files, _ := filepath.Glob(filepath.Join(a.Dir(cfg), "sessions", "*.json"))
		regFiles += len(files)
		for _, e := range sessions.ReadRegistry(a.Name, a.Dir(cfg)) {
			if e.OK {
				regOK++
			}
		}
	}
	if regFiles == 0 {
		skip("registry: not tested", "no live-session records right now")
	} else {
		// Every claim file must parse, not merely one: a single malformed
		// record is a session the dd guard can no longer see.
		chk(regOK == regFiles, "registry: live-session records carry sessionId + pid + startedAt",
			fmt.Sprintf("%d of %d records unparseable — those sessions lost the dd guard", regFiles-regOK, regFiles))
	}

	// Transcripts, through the same collector the picker uses. Per-file
	// absence of a title is ordinary (print-mode sessions); a store where
	// *nothing* titles or *nothing* cwd-verifies means the record shapes
	// drifted.
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.ProjectsDir(),
		CWD:         cfg.Home,
		OwnersPath:  cfg.OwnersFile(),
	})
	if len(listing.Sessions) == 0 {
		skip("sessions: not tested", "store is empty")
	} else {
		titled, cwdOK := 0, 0
		for _, s := range listing.Sessions {
			if s.Tail.Title() != "" {
				titled++
			}
			if s.CWD != "" {
				cwdOK++
			}
		}
		chk(titled > 0, "sessions: title/prompt records parse via shared contract",
			"no transcript yields a title — record shapes drifted")
		chk(cwdOK > 0, "sessions: cwd records verify against store dir names",
			"no transcript's cwd munges to its dir — resume targets are gone")

		// The tail budget is the assertion most likely to actually fire, and
		// it must test the whole claim: for every transcript larger than the
		// budget, the tail-derived title *and* cwd must equal what a
		// full-file parse resolves — a custom rename beyond the budget with
		// a newer last-prompt inside it renders the wrong title while "some
		// title exists" still passes. The comparison runs through the shared
		// parser, never a substring scan: transcripts about these tools
		// quote the record types verbatim in message bodies, and a raw
		// needle match reads that as a title (it did, on this very repo's
		// own sessions).
		over := 0
		for _, s := range listing.Sessions {
			if s.Size <= sessions.TailBudget {
				continue // the tail pass already saw the whole file
			}
			data, err := os.ReadFile(s.Path)
			if err != nil {
				continue
			}
			full := sessions.ParseTail(data, filepath.Base(s.StoreDir), true)
			if full.Title() != s.Tail.Title() || full.CWD != s.CWD || full.Model != s.Tail.Model {
				over++
			}
		}
		chk(over == 0, "sessions: the adaptive tail reader resolves the same title/cwd/model as a full parse",
			fmt.Sprintf("%d transcript(s) resolve differently — a record drifted out of the reader's reach", over))
	}

	// .owners is headroom's own file, but a corrupt one silently reads as
	// empty (by design, so the picker keeps working) — check is where that
	// loss becomes visible instead of staying silent. Through the same
	// parser routing uses: a checker-local decode once passed a document
	// the loader was rejecting wholesale.
	if data, err := os.ReadFile(cfg.OwnersFile()); err == nil && len(data) > 0 {
		_, perr := sessions.ParseOwners(data)
		chk(perr == nil, "owners: re-home records parse via shared contract",
			"file corrupt — explicit re-homes are being ignored")
	}
}

// tokenCheck is what a re-read of the Keychain can establish about a 401.
type tokenCheck int

const (
	tokenSame      tokenCheck = iota // the token we spent is still the stored one
	tokenChanged                     // Claude Code refreshed it under us — a race, not drift
	tokenUncertain                   // the re-read told us nothing
)

// recheckToken compares the stored access token against the one a request was
// actually made with. The three outcomes stay distinct because "I could not
// re-read the credential" is not evidence of a refresh: reporting it as one
// would have check assert a race it never observed.
func recheckToken(configDir, used string) tokenCheck {
	blob, ok := creds.Parse(creds.ReadKeychain(configDir))
	switch {
	case !ok:
		return tokenUncertain
	case blob.Token != used:
		return tokenChanged
	default:
		return tokenSame
	}
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
