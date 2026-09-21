package accountstate

import (
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

type Account struct {
	Acct accounts.Account
	Key  state.Key
	View Facts
}
type Snapshot struct {
	Accounts []Account
	Current  string
	Store    state.Snapshot
}

func Read(scope config.Scope, st *state.Store, now time.Time) Snapshot {
	snap := st.Load()
	list, current := Assemble(accounts.Discover(scope), snap, now)
	return Snapshot{list, current, snap}
}

// Assemble selects strict current-account state and the newest usable observation.
func Assemble(set accounts.Set, snap state.Snapshot, now time.Time) ([]Account, string) {
	current := ""
	if a, err := set.Select(""); err == nil {
		current = a.Name
	}
	list := make([]Account, 0, len(set.Accounts))
	for _, a := range set.Accounts {
		key := state.Key{UUID: a.AccountID, Name: a.Name}
		v := Facts{Vendor: a.Scope.Vendor, FreshFor: a.Scope.RequestSpacing(), Label: a.Name, Launcher: accounts.Launcher(a), Current: current == a.Name, Health: HealthUnprobed}
		if a.Email != "" {
			v.Label = a.Email
		}
		if !a.IsPrimary() && a.Email != "" && a.Email != a.Name {
			v.DirMismatch = a.Name
		}
		if a.Scope.Vendor == config.Codex && !a.Readable {
			// A Codex account that cannot say whose it is has no ledger key at
			// all: it replays nothing, even if an earlier login on this home
			// was fetched, and it has no deadline to report.
			list = append(list, Account{a, key, v})
			continue
		}
		v.Obs = newestObservation(snap, key, a, now)
		if next := snap.NextEligible(key, now); next.After(now) {
			v.Attempt.NextEligibleAt = next.Unix()
		}
		list = append(list, Account{a, key, v})
	}
	return list, current
}

// newestObservation replays through the one parse dispatch: the store a body
// was read from says which vendor's parser reads it, and the same response
// identity check that guards a live body guards a replayed one. Usage is never
// derived from a vendor's session files — a Codex rollout carries no account
// identity, and under the shared store one rollout holds several accounts'
// turns — so a Codex account has only headroom's own stored response here.
func newestObservation(snap state.Snapshot, k state.Key, a accounts.Account, now time.Time) *Observation {
	var best *Observation
	accountID, userID := a.ResponseIdentity()
	consider := func(body []byte, atMS int64, source Source) {
		if len(body) == 0 || atMS <= 0 {
			return
		}
		// Zero rows is as much an answer here as it is from the live endpoint
		// — "this account reported no limit windows at time X" beats showing
		// nothing at all.
		reading, err := usage.Parse(snap.Vendor(), body)
		if err != nil || !reading.BelongsTo(accountID, userID) {
			return
		}
		at := atMS / 1000
		if best != nil && at <= best.ObservedAt {
			return
		}
		best = FromReading(reading, at, source)
	}
	if snap.Vendor() != a.Scope.Vendor {
		return nil
	}
	if obs, ok := snap.Observation(k, now); ok {
		consider(obs.Body, obs.FetchedAtMS, SourceStore)
	}
	consider(a.Meta.CachedUsage, a.Meta.FetchedAtMS, SourceCache)
	return best
}
