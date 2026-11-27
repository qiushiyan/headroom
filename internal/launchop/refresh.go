package launchop

import (
	"time"

	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
)

// worthRefreshing reports whether a refresh would find anybody to ask right
// now: an account whose quiet period has passed and whose stored token is
// usable, or a machine the scope pulls readings from whose pull is due —
// which, on a machine whose own tokens have all aged out, is the only way its
// figures move. The claims still decide; this only keeps a placement from
// spawning a process that could ask nobody. Whether a placement leaves a
// refresh at all is its own to decide (a choice the rule made).
func worthRefreshing(f facts, now time.Time) bool {
	for _, c := range f.cands {
		if c.askable(now) && !f.snap.NextEligible(c.key, now).After(now) {
			return true
		}
	}
	for _, peer := range f.set.Scope.Peers {
		if !f.snap.NextPull(peer, now).After(now) {
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
	switch a.Scope.Vendor {
	case config.Codex:
		return a.Auth.State == codexauth.OK && !a.Auth.TokenStale(now.Unix())
	case config.Claude:
		blob, ok := creds.Parse(c.cred)
		return ok && a.Readable && blob.TokenUsable(now.UnixMilli())
	}
	// Antigravity is never placed, so never a candidate.
	return false
}
