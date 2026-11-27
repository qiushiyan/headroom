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

// Snapshot is one disk read: the discovered set, what is known about each of
// its accounts, and the store they were read against. The set travels with
// the facts so that a placement is asked of the accounts that were actually
// discovered, never of a set rebuilt beside them.
type Snapshot struct {
	Set      accounts.Set
	Accounts []Account
	Store    state.Snapshot
}

func Read(scope config.Scope, st *state.Store, now time.Time) Snapshot {
	snap := st.Load()
	set := accounts.Discover(scope)
	return Snapshot{set, Assemble(set, snap, now), snap}
}

// Assemble is every discovered account with the newest usable observation.
func Assemble(set accounts.Set, snap state.Snapshot, now time.Time) []Account {
	list := make([]Account, 0, len(set.Accounts))
	for _, a := range set.Accounts {
		key := state.KeyFor(a)
		v := Facts{Vendor: a.Scope.Vendor, FreshFor: FreshFor(a.Scope), Label: a.Name, Launcher: accounts.Launcher(a), Health: HealthUnprobed}
		if a.Email != "" {
			v.Label = a.Email
		}
		if !a.IsPrimary() && a.Email != "" && a.Email != a.Name {
			v.DirMismatch = a.Name
		}
		switch a.Scope.Vendor {
		case config.Codex:
			v.LoginCommand = accounts.EngineLauncher(a) + " -- login"
			// What the login's id token says, until a response names a plan.
			v.Plan = a.Auth.Plan
			if !a.Readable {
				// A Codex account that cannot say whose it is has no ledger
				// key at all: it replays nothing, even if an earlier login on
				// this home was fetched, and it has no deadline to report.
				list = append(list, Account{a, key, v})
				continue
			}
		case config.Agy:
			// agy signs in with its own browser flow, started by running it.
			v.LoginCommand = "agy"
		}
		// Decided here, from disk alone, so `limits` reports the figures,
		// plan and label the board does.
		v.Show(Select(snap, key, a, now))
		if next := snap.NextEligible(key, now); next.After(now) {
			v.Attempt.NextEligibleAt = next.Unix()
		}
		list = append(list, Account{a, key, v})
	}
	return list
}

// Select replays through the one parse dispatch: the store a body
// was read from says which vendor's parser reads it, and the same response
// identity check that guards a live body guards a replayed one. Usage is never
// derived from a vendor's session files — a Codex rollout carries no account
// identity, and under the shared store one rollout holds several accounts'
// turns — so a Codex account has only headroom's own stored response here.
//
// Beside this machine's own readings stand those of the machines it pulls
// from, for the same account by identity, and the newest of them all wins
// (Newest): a tie goes to this machine's own.
//
// Select is that choice for one account, from one snapshot: the assembly
// makes it, and a round makes it again over what a pull has just recorded.
func Select(snap state.Snapshot, k state.Key, a accounts.Account, now time.Time) *Observation {
	best, _ := ownNewest(snap, k, a, now)
	for _, peer := range a.Scope.Peers {
		best = Newest(best, PeerObservation(snap, k, a, peer, now))
	}
	return best
}

// ownNewest is the newest reading this machine itself holds of the account —
// headroom's stored fetch, or Claude Code's cache — and the body it was read
// from, as it was stored.
func ownNewest(snap state.Snapshot, k state.Key, a accounts.Account, now time.Time) (*Observation, state.Observation) {
	var best *Observation
	var raw state.Observation
	consider := func(body []byte, atMS int64, source Source) {
		if len(body) == 0 || atMS <= 0 {
			return
		}
		// Zero rows is as much an answer here as it is from the live endpoint
		// — "this account reported no limit windows at time X" beats showing
		// nothing at all.
		reading, ok := readBody(snap, a, body)
		if !ok {
			return
		}
		if o := FromReading(reading, atMS/1000, source); Newest(best, o) == o {
			best, raw = o, state.Observation{FetchedAtMS: atMS, Body: body}
		}
	}
	if snap.Vendor() != a.Scope.Vendor {
		return nil, raw
	}
	if obs, ok := snap.Observation(k, now); ok {
		consider(obs.Body, obs.FetchedAtMS, SourceStore)
	}
	consider(a.Meta.CachedUsage, a.Meta.FetchedAtMS, SourceCache)
	return best, raw
}

// PeerObservation is the reading peer last gave of the account, through the
// one parse dispatch and the response identity check a body of this
// machine's own goes through; nil when it gave none that is usable. What
// matches it to the account is the identity the ledger keys on, never a name.
func PeerObservation(snap state.Snapshot, k state.Key, a accounts.Account, peer string, now time.Time) *Observation {
	if snap.Vendor() != a.Scope.Vendor {
		return nil
	}
	o, ok := snap.PeerObservation(peer, k, now)
	if !ok {
		return nil
	}
	reading, ok := readBody(snap, a, o.Body)
	if !ok {
		return nil
	}
	obs := FromReading(reading, o.FetchedAtMS/1000, SourcePeer)
	obs.Peer = peer
	return obs
}

func readBody(snap state.Snapshot, a accounts.Account, body []byte) (usage.Reading, bool) {
	accountID, userID := a.ResponseIdentity()
	reading, err := usage.Parse(snap.Vendor(), body)
	return reading, err == nil && reading.BelongsTo(accountID, userID)
}

// OwnReading is one account's newest reading as this machine itself holds
// it, as stored: what another machine pulls.
type OwnReading struct {
	Key  state.Key
	Name string
	Obs  state.Observation
}

// OwnReadings is the newest reading this machine itself holds of every
// account in set that is known by its identity — never one it pulled from
// another machine, so no reading travels back to where it came from under a
// second machine's name. An account known only by its dir's name has none: a
// name says nothing about which account another machine's dir of that name
// holds.
func OwnReadings(set accounts.Set, snap state.Snapshot, now time.Time) []OwnReading {
	var out []OwnReading
	for _, a := range set.Accounts {
		k := state.KeyFor(a)
		if k.UUID == "" || !a.Readable {
			continue
		}
		if obs, raw := ownNewest(snap, k, a, now); obs != nil {
			name := a.Name
			if a.Email != "" {
				name = a.Email
			}
			out = append(out, OwnReading{Key: k, Name: name, Obs: raw})
		}
	}
	return out
}
