package launchop

import (
	"time"

	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
)

// worthRefreshing reports whether any account could be asked right now: its
// quiet period has passed and its stored token is usable. The claim still
// decides; this only keeps a placement from spawning a process that could ask
// nobody — which, on a machine whose idle accounts' tokens have aged out,
// would otherwise be every launch. Whether a placement leaves a refresh at all
// is its own to decide (a choice the rule made); this is only whether one
// would find anybody to ask.
func worthRefreshing(f facts, now time.Time) bool {
	for _, c := range f.cands {
		if c.askable(now) && !f.snap.NextEligible(c.key, now).After(now) {
			return true
		}
	}
	return false
}

// askable reports whether the account's stored login could carry a usage
// request right now. For Claude Code that is the credential the placement
// read, which an unread or unreadable one never is; Codex's is the auth.json
// snapshot discovery took.
func (c candidate) askable(now time.Time) bool {
	a := c.acct
	if a.Scope.Vendor == config.Codex {
		return a.Auth.State == codexauth.OK && !a.Auth.TokenStale(now.Unix())
	}
	blob, ok := creds.Parse(c.cred)
	return ok && a.Readable && blob.TokenUsable(now.UnixMilli())
}
