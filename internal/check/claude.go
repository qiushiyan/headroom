package check

// The Claude Code group: the installed binary's seams, each account's
// credential and a live usage request through the board's own request path,
// the identity and health documents the board reads, then headroom's own
// files, routing, homes, retention and the session store.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

func checkClaude(scope config.Scope, environ []string, probe sessions.PIDProbe, r *report) {
	// The installed binary still carries the seams this tooling depends on:
	// the usage endpoint, config-dir routing, and the credentials service
	// name.
	bin, binName := claudeBinary(), ""
	var missing []string
	hint := "missing: binary not found"
	if bin != "" {
		binName = filepath.Base(bin)
		missing = missingSeams(bin, []string{"api/oauth/usage", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "-credentials"})
		hint = "missing: " + strings.Join(missing, " ")
	}
	r.check(bin != "" && len(missing) == 0,
		fmt.Sprintf("binary (%s): endpoint + config-dir + credential seams present", binName), hint)

	// Resolve credentials exactly as the board does, and parse the selected
	// blob through its shared contract. Fetches run in parallel and are
	// validated after the join, so a dead network costs one timeout.
	set := accounts.Discover(scope)
	accts := set.Accounts
	now := time.Now()
	st := state.Open(scope)
	requests := make([]*refresh.Candidate, len(accts))
	for i, a := range accts {
		if !a.IsPrimary() && !hasMeta(a) {
			continue
		}
		raw, source := creds.ReadRaw(context.Background(), a.ConfigDir, a.Dir())
		blob, ok := creds.Parse(raw)
		hint := "credential blob did not parse (accessToken missing or format changed)"
		if source == creds.SourceNone {
			hint = "not logged in — no Keychain item or .credentials.json"
		}
		r.check(ok, fmt.Sprintf("credential[%s]: %s", display(a), credentialSourceLabel(source)), hint)
		candidate, eligibility := refresh.Prepare(a, blob, ok, now)
		requests[i] = candidate
		if candidate == nil {
			reason := "credential blob did not parse"
			switch eligibility {
			case accountstate.AttemptTokenStale:
				reason = fmt.Sprintf("access token stale — any %s session refreshes it", accounts.Launcher(a))
			case accountstate.AttemptIdentityUnknown:
				reason = "account identity unreadable — request budget cannot be identified"
			}
			r.skip(fmt.Sprintf("api[%s]: not tested", display(a)), reason)
		}
	}
	results := make([]refresh.Result, len(accts))
	for res := range refresh.Start(context.Background(), st, requests, rereadCredential(scope.PrimaryDir())) {
		results[res.Index] = res
	}
	for i, candidate := range requests {
		if candidate != nil {
			r.request(display(accts[i]), results[i], now)
		}
	}

	// .claude.json still records the logged-in email (dashboard labels).
	_, ok := accounts.MetaEmail(scope.PrimaryMeta())
	r.check(ok, "claude.json: .oauthAccount.emailAddress present", "")

	// The two surfaces this tool grew to depend on are checked through the
	// same parsers rendering uses, or the checker would pass while the
	// dashboard silently fell back to guesses.
	for _, a := range accts {
		if !a.IsPrimary() && !hasMeta(a) {
			continue
		}
		name := display(a)
		switch auth.Query(a.ConfigDir).Outcome {
		case auth.OutcomeOK:
			r.ok(fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name))
		case auth.OutcomeUnparseable:
			r.fail(fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name),
				"output shape drifted — account health can no longer be established")
		case auth.OutcomeUnrunnable:
			// Not the vendor's problem: launch refused to build the probe's
			// environment (a dir it will not hand to CLAUDE_CONFIG_DIR).
			// The dirs-absolute assertion in checkRouting names the dir.
			r.own(false, fmt.Sprintf("auth[%s]: probe environment could not be constructed", name),
				"this account's config dir was refused by the launch seam")
		default:
			r.skip(fmt.Sprintf("auth[%s]: not tested", name), "claude auth status unavailable")
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
			r.fail(cacheLabel,
				"cached payload present but its account identity or fetch time is missing — fallback lost")
			continue
		}
		rows, err := usage.ParseLimits(a.Meta.CachedUsage)
		nbad := 0
		for _, row := range rows {
			if row.Drifted() {
				nbad++
			}
		}
		r.check(err == nil && nbad == 0, cacheLabel,
			"cached payload shape drifted — the offline fallback is unreadable")
	}

	// One read of headroom's own files, after this run's requests recorded
	// theirs, for every assertion that reads them.
	snap := st.Load()
	checkOwnState(snap, r)
	// Deliberately outside checkOwnState: these depend on .current and the
	// process environment, not on state.json, so a state document written by
	// a newer headroom (which rightly short-circuits the state audit) must
	// not silence them.
	checkRouting(set, environ, r)
	checkHomes(set, snap, r)
	checkRetention(set, r)
	checkSessionStore(scope, accts, snap, probe, r)
}

// hasMeta reports an account whose .claude.json Claude Code has written; an
// extra seeded and never run has nothing to check yet.
func hasMeta(a accounts.Account) bool {
	_, err := os.Stat(a.MetaPath())
	return err == nil
}

func credentialSourceLabel(source creds.Source) string {
	switch source {
	case creds.SourceKeychain:
		return "keychain item under predicted name"
	case creds.SourceFile:
		return ".credentials.json (no keychain item)"
	default:
		return "no credential source"
	}
}

// rereadCredential samples the same source the board and initial check read.
func rereadCredential(primaryDir string) refresh.Reread {
	return func(keychainKey string) (string, bool) {
		dir := keychainKey
		if dir == "" {
			dir = primaryDir
		}
		raw, _ := creds.ReadRaw(context.Background(), keychainKey, dir)
		blob, ok := creds.Parse(raw)
		return blob.Token, ok
	}
}
