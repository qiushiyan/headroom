package launchop

import (
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/state"
)

// Marks is what one gathering for a board's marks read: the candidates and
// the record of launches. The choice they decide moves with the clock alone —
// a window ends, a launch stops counting as load, a weekly reset comes nearer
// and reorders a tie — so a board that stays open asks again from them at
// every frame's time. What they were read from — credentials, live sessions,
// the record — is as fresh as the gathering, and a launch made from another
// terminal since then shows at the next.
type Marks struct {
	cands  []placement.Candidate
	ledger placement.Ledger
	// refused says a launch would refuse — a reservation that cannot be read
	// — so no account is the one it would take.
	refused bool

	// reserved is what each reserved account is reserved for, by name.
	reserved map[string]string
}

// Mark gathers what a board's marks are decided from: the load a launch would
// count on each account — its busy sessions and recent launches — and the
// account a launch in dir would take from the figures the board is showing: a
// reservation makes that depend on the directory, and the board's is the
// launch it can speak for.
//
// The mark is advice: it is computed from the board's observations and the
// record as it stood, and a launch decides again from the disk — counting
// whatever was placed in between. What it is not allowed to be is a second
// opinion: the candidates are built by the builder a launch uses. nil when
// there is nothing to mark.
func Mark(set accounts.Set, st *state.Store, accts []accountstate.Account, environ []string, dir string, now time.Time, deps Deps) *Marks {
	if len(accts) == 0 || st == nil {
		return nil
	}
	snap := st.Load()
	f := build(set, st, snap, sources(accts), readReservations(set.Scope), environ, dir, true, now, deps)
	reserved := map[string]string{}
	for _, c := range f.cands {
		if c.reserved != "" {
			reserved[c.Name] = c.reserved
		}
	}
	return &Marks{cands: f.rule(), ledger: snap.Placements(), refused: f.reservedErr != nil, reserved: reserved}
}

// At is every account's mark at now, by name: its load and whether it is the
// one a launch would take. A nil Marks marks nothing.
func (m *Marks) At(now time.Time) map[string]accountstate.Mark {
	if m == nil {
		return nil
	}
	decision := placement.Choose(m.cands, m.ledger, placement.Intent{}, now)
	out := make(map[string]accountstate.Mark, len(decision.Candidates))
	for _, c := range decision.Candidates {
		out[c.Name] = accountstate.Mark{
			Next:     !m.refused && decision.Chosen != "" && c.Name == decision.Chosen,
			Load:     &accountstate.Load{Value: c.Load, Busy: c.Busy, Launched: c.Pending},
			Reserved: m.reserved[c.Name],
		}
	}
	return out
}
