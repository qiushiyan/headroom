// Package check verifies the assumptions headroom reverse-engineered from
// Claude Code. Each FAIL means Claude Code likely changed a format on
// update. Checks go through the exact same parsers rendering uses, so the
// checker cannot drift from what rendering actually needs.
package check

import (
	"bytes"
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
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/usage"
)

func Run(cfg config.Config, out io.Writer, color bool) int {
	p := render.NewPalette(color)
	fails := 0
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
	nowMS := time.Now().UnixMilli()
	client := &http.Client{Timeout: 10 * time.Second}

	const (
		modeNeverLoggedIn = iota // nothing to verify
		modeSkipped              // logged in but no usable token
		modeFetched
	)
	type apiCase struct {
		name   string
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
		c := &apiCase{name: name, mode: modeSkipped}
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

		if ok && !blob.Expired(nowMS) {
			c.mode = modeFetched
			wg.Add(1)
			go func(c *apiCase, token string) {
				defer wg.Done()
				c.res = usage.Fetch(client, cfg.UsageURL, token)
			}(c, blob.Token)
		} else {
			c.reason = fmt.Sprintf("token missing or expired — run %s first",
				accounts.Launcher(a, accts, cfg.PrimaryName))
		}
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
			chk(false, fmt.Sprintf("api[%s]: skipped", c.name), c.reason)
			continue
		}
		code := c.res.StatusCode
		var rows []usage.Row
		if c.res.Err == nil && code == http.StatusOK {
			if r, err := usage.ParseLimits(c.res.Body); err == nil {
				rows = r
			}
		}
		nbad := 0
		for _, r := range rows {
			if r.Drifted() {
				nbad++
			}
		}
		hint := ""
		switch {
		case code != http.StatusOK:
			hint = "non-200 — endpoint or auth drifted"
		case len(rows) == 0:
			hint = "response unparseable or no limits — shape drifted"
		case nbad > 0:
			hint = fmt.Sprintf("%d malformed field(s) — shape drifted", nbad)
		}
		codeStr := "n/a"
		if code != 0 {
			codeStr = strconv.Itoa(code)
		}
		chk(code == http.StatusOK && len(rows) >= 1 && nbad == 0,
			fmt.Sprintf("api[%s]: HTTP %s, %d row(s), fields well-formed", c.name, codeStr, len(rows)), hint)
	}

	// .claude.json still records the logged-in email (dashboard labels).
	_, ok := accounts.MetaEmail(cfg.PrimaryMeta())
	chk(ok, "claude.json: .oauthAccount.emailAddress present", "")

	fmt.Fprintln(out)
	if fails > 0 {
		fmt.Fprintf(out, "%d check(s) failed — Claude Code likely changed a format\n", fails)
		return 1
	}
	fmt.Fprintln(out, "all checks passed")
	return 0
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

// searchFile streams the (large) binary in chunks, keeping an overlap so a
// needle split across a chunk boundary is still found.
func searchFile(path string, needles []string) map[string]bool {
	found := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return found
	}
	defer f.Close()

	overlap := 0
	for _, n := range needles {
		if len(n) > overlap {
			overlap = len(n)
		}
	}
	buf := make([]byte, 4<<20)
	var tail []byte
	for {
		n, err := f.Read(buf)
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
