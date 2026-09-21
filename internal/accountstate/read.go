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
		v.Obs = newestObservation(snap, key, a.Meta, now)
		if next := snap.NextEligible(key, now); next.After(now) {
			v.Attempt.NextEligibleAt = next.Unix()
		}
		list = append(list, Account{a, key, v})
	}
	return list, current
}

func newestObservation(snap state.Snapshot, k state.Key, meta accounts.Meta, now time.Time) *Observation {
	var best *Observation
	consider := func(body []byte, atMS int64, source Source) {
		if len(body) == 0 || atMS <= 0 {
			return
		}
		// Zero rows is as much an answer here as it is from the live endpoint
		// — "this account reported no limit windows at time X" beats showing
		// nothing at all.
		rows, err := usage.ParseLimits(body)
		if err != nil {
			return
		}
		at := atMS / 1000
		if best != nil && at <= best.ObservedAt {
			return
		}
		best = &Observation{Rows: rows, ObservedAt: at, Source: source}
	}
	if obs, ok := snap.Observation(k, now); ok {
		consider(obs.Body, obs.FetchedAtMS, SourceStore)
	}
	consider(meta.CachedUsage, meta.FetchedAtMS, SourceCache)
	return best
}
