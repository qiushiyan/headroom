// Package sessions reads what Claude Code records about its sessions, for a
// launch to weigh: which account drove a session (its owner), which sessions
// are running now and on which account (the live registry), and which
// repository a directory belongs to. Transcripts are machine-global, kept in
// one store every account links to; nothing here reads them beyond their ids.
package sessions

// AccountRef names one discovered account and its real config dir.
type AccountRef struct {
	Name string
	Ref  string // how a re-home names it (accounts.Account.Ref)
	Dir  string
}

// OwnerOf names the account that last drove a session, for a launch that
// names the session by id and must decide where it runs: the newest account
// claim headroom can observe or was explicitly given — not unobservable ground
// truth. A verified live registry entry outranks everything (that account is
// driving the session at this instant); otherwise the explicit re-home and
// each account's newest prompt for the session compete on one honest axis,
// event time on this machine's clock. Nothing is ever re-stamped, so stale
// evidence cannot be promoted.
//
// It is "" when nothing names an owner: no evidence, two accounts claiming
// the same newest moment, or a newest re-home to an account that no longer
// exists — which must not fall through to older history, since that would
// resurrect a routing the person abandoned. A launch then places the session
// like a new one, never on the primary by default.
//
// evidence is that session's process evidence from the caller's one registry
// inspection; owners are the explicit re-homes.
func OwnerOf(id string, accts []AccountRef, evidence ProcessEvidence, owners map[string]OwnerRec) string {
	if evidence.State == Live {
		return evidence.Account
	}
	byRef, hists := map[string]string{}, map[string]History{}
	for _, a := range accts {
		byRef[a.Ref] = a.Name
		if h, ok := ReadHistory(a.Dir); ok {
			hists[a.Name] = h
		}
	}
	return newestClaim(id, byRef, hists, owners)
}

// newestClaim is the owner rule over evidence already read: the account each
// ref is called now, each account's prompt history and the explicit re-homes.
func newestClaim(id string, byRef map[string]string, hists map[string]History, owners map[string]OwnerRec) string {
	best, bestTS, conflict := "", int64(0), false
	consider := func(acct string, ts int64) {
		switch {
		case ts > bestTS:
			best, bestTS, conflict = acct, ts, false
		case ts == bestTS && ts > 0 && acct != best:
			// Two accounts claiming the same newest moment is undecidable,
			// whatever the sources.
			conflict = true
		}
	}
	if rec, ok := owners[id]; ok {
		// A re-home to an account that is gone is "" here, and still the
		// newest word on the session.
		consider(byRef[rec.Account], rec.AtMS)
	}
	for acct, h := range hists {
		if ts, ok := h.Newest[id]; ok {
			consider(acct, ts)
		}
	}
	if conflict {
		return ""
	}
	return best
}
