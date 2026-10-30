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
	home   string
	labels state.HomeLabels
	auto   bool

	// reserved is what each reserved account is reserved for, by name.
	reserved map[string]string
}

// Mark gathers what a board's marks are decided from: the load an automatic
// launch would count on each account — its busy sessions and recent launches,
// from every home sharing the ledger — and, under auto, the account such a
// launch in dir would take from the figures the board is showing: a
// reservation makes that depend on the directory, and the board's is the
// launch it can speak for. mode is the board's own reading of `.current`,
// taken once with its facts.
//
// The mark is advice: it is computed from the board's observations and the
// record as it stood, and a launch decides again from the disk — counting
// whatever was placed in between. What it is not allowed to be is a second
// opinion: the candidates are built by the builder a launch uses. nil when
// there is nothing to mark.
func Mark(set accounts.Set, st *state.Store, accts []accountstate.Account, mode string, environ []string, dir string, now time.Time, deps Deps) *Marks {
	if len(accts) == 0 || st == nil {
		return nil
	}
	snap := st.Load()
	auto := mode == "auto"
	f := build(set, st, snap, sources(accts), environ, dir, auto, now, deps)
	reserved := map[string]string{}
	for _, c := range f.cands {
		if c.reserved != "" {
			reserved[c.Name] = c.reserved
		}
	}
	// A launch would refuse, so no account is the one it would take.
	auto = auto && f.reservedErr == nil
	return &Marks{cands: f.rule(), ledger: snap.Placements(), home: st.Home(), labels: f.labels, auto: auto, reserved: reserved}
}

// At is every account's mark at now, by name: its load and, under auto,
// whether it is the one a launch would take. A nil Marks marks nothing.
func (m *Marks) At(now time.Time) map[string]accountstate.Mark {
	if m == nil {
		return nil
	}
	decision := placement.Choose(m.cands, m.ledger, placement.Intent{}, now)
	out := make(map[string]accountstate.Mark, len(decision.Candidates))
	for _, c := range decision.Candidates {
		out[c.Name] = accountstate.Mark{
			Next:     m.auto && decision.Chosen != "" && c.Name == decision.Chosen,
			Load:     loadFacts(c, m.home, m.labels),
			Reserved: m.reserved[c.Name],
		}
	}
	return out
}

// loadFacts is a counted candidate's load as a surface reports it.
func loadFacts(c placement.Counted, self string, labels state.HomeLabels) *accountstate.Load {
	l := &accountstate.Load{Value: c.Load, Busy: c.Busy, Launched: c.Pending}
	for _, sh := range c.Shares {
		l.Homes = append(l.Homes, accountstate.HomeLoad{
			Home: sh.Home, Label: labels.Of(sh.Home), This: sh.Home == self, Busy: sh.Busy, Launched: sh.Pending,
		})
	}
	return l
}
